package bluehost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	contractfixtures "github.com/ZabLaboratory/Blue/runtime/go/contractfixtures"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

func TestHTTPTransportAdaptersShareResponseAndSecurity(t *testing.T) {
	type observation struct {
		query         url.Values
		authorization string
		forwarded     string
	}
	observations := make(chan observation, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observations <- observation{
			query:         r.URL.Query(),
			authorization: r.Header.Get("Authorization"),
			forwarded:     r.Header.Get("X-Transport-Test"),
		}
		w.Header().Set("X-Transport-Result", "shared")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":3,"ok":true}`))
	}))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	egress := effects.NewEgressPolicy([]string{serverURL.Hostname()}, true).InsecureAllowPrivateForTest()
	request := map[string]any{
		"url":     server.URL + "?from=base",
		"method":  "GET",
		"query":   map[string]any{"q": "blue host"},
		"headers": map[string]any{"Authorization": "must-not-forward", "X-Transport-Test": "kept"},
	}

	direct, err := doHTTPRequest(context.Background(), egress, request)
	if err != nil {
		t.Fatalf("direct EffectHandlers transport: %v", err)
	}
	genericResult := runHTTPEffect(context.Background(), egress, map[string]any{"request": request})
	if genericResult.Err != "" {
		t.Fatalf("generic invocation transport: %s", genericResult.Err)
	}
	genericValue, err := decodeCanonicalJSON(genericResult.Value)
	if err != nil {
		t.Fatalf("decode generic response: %v", err)
	}
	generic, ok := genericValue.(map[string]any)
	if !ok {
		t.Fatalf("generic response shape: %#v", genericValue)
	}

	for _, key := range []string{"status", "body", "headers"} {
		if !reflect.DeepEqual(direct[key], generic[key]) {
			t.Fatalf("shared transport response diverged for %q: direct=%#v generic=%#v", key, direct[key], generic[key])
		}
	}
	if ok, _ := direct["ok"].(bool); !ok {
		t.Fatalf("direct response did not retain its EffectHandlers ok output: %#v", direct["ok"])
	}

	for i := 0; i < 2; i++ {
		observation := <-observations
		if got := observation.query.Get("from"); got != "base" {
			t.Fatalf("request %d lost the original query parameter: %q", i, got)
		}
		if got := observation.query.Get("q"); got != "blue host" {
			t.Fatalf("request %d lost the authored query parameter: %q", i, got)
		}
		if observation.authorization != "" {
			t.Fatalf("request %d forwarded Authorization through the shared transport", i)
		}
		if observation.forwarded != "kept" {
			t.Fatalf("request %d dropped a permitted authored header", i)
		}
	}
}

func TestHTTPTransportAdaptersShareHeaderCap(t *testing.T) {
	egress := effects.NewEgressPolicy([]string{"allowed.example.test"}, false)
	request := map[string]any{
		"url":     "https://allowed.example.test/effect",
		"headers": map[string]any{"X-Oversized": strings.Repeat("x", maxHTTPTransportHeaders)},
	}

	if _, err := doHTTPRequest(context.Background(), egress, request); err == nil || !strings.Contains(err.Error(), "HTTP_REQUEST_HEADERS_TOO_LARGE") {
		t.Fatalf("direct adapter did not enforce the shared header cap: %v", err)
	}
	generic := runHTTPEffect(context.Background(), egress, map[string]any{"request": request})
	if !strings.Contains(generic.Err, "HTTP_REQUEST_HEADERS_TOO_LARGE") {
		t.Fatalf("generic adapter did not enforce the shared header cap: %s", generic.Err)
	}
}

// TestHTTPTransportMethodNormalization proves, by execution, the `method`
// parity table Blue copied into HttpRequestPayload (Blue PR #330, mirroring
// http_transport.go:78-81 + the token-validation follow-through at ~:101).
// Orion#385: the Blue side of this contract was proven by Python unit tests
// only — nothing on the Go side asserted `method` at all. Every assertion
// here reads the method the httptest.Server actually received on the wire
// (or the absence of any request, for the refused case), never a value the
// test recomputes itself — a self-referential oracle would prove nothing.
func TestHTTPTransportMethodNormalization(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		onWire  string // expected r.Method; ignored when refused is true
		refused bool   // true: the transport must error and the server must never see a request
	}{
		{name: "empty defaults to GET", method: "", onWire: http.MethodGet},
		{name: "lowercase get uppercases", method: "get", onWire: http.MethodGet},
		{name: "mixed case uppercases", method: "GeT", onWire: http.MethodGet},
		{name: "lowercase post uppercases", method: "post", onWire: http.MethodPost},
		{name: "non-standard verb PURGE is not allowlisted away", method: "PURGE", onWire: "PURGE"},
		{name: "non-standard verb PROPFIND is not allowlisted away", method: "PROPFIND", onWire: "PROPFIND"},
		{name: "padded method is refused, never reaches the wire", method: " get ", refused: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Method
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			serverURL, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			egress := effects.NewEgressPolicy([]string{serverURL.Hostname()}, true).InsecureAllowPrivateForTest()
			request := map[string]any{"url": server.URL, "method": tc.method}

			_, err = doHTTPRequest(context.Background(), egress, request)

			if tc.refused {
				if err == nil {
					t.Fatalf("method %q: expected the transport to refuse it, got no error", tc.method)
				}
				select {
				case got := <-received:
					t.Fatalf("method %q: transport returned an error (%v) but the server still received a request with method %q — it was emitted anyway", tc.method, err, got)
				default:
				}
				return
			}

			if err != nil {
				t.Fatalf("method %q: unexpected transport error: %v", tc.method, err)
			}
			select {
			case got := <-received:
				if got != tc.onWire {
					t.Fatalf("method %q: server observed method %q on the wire, want %q", tc.method, got, tc.onWire)
				}
			default:
				t.Fatalf("method %q: server never received a request", tc.method)
			}
		})
	}
}

// TestHTTPTransportObservesURLTimeoutAndBodyOnWire extends the parity
// TestHTTPTransportMethodNormalization already established for `method` to
// the three other core.http.request@1 fields ADR 013 T3 requires this arm to
// observe on the real wire: url, timeout_ms, and the body — including its
// bound. Every assertion reads what the httptest.Server actually received
// (or the confirmed absence of a request, for the refused case), never a
// value the test recomputes itself (§3.3).
//
// ADR 013 §3.6, normative: the fixture fixes the SHAPE of `body`, never a
// numeric byte cap — that is egress/host policy, and this test asserts no
// value as if Blue had decreed it. What it DOES assert is Orion's own real
// enforcement: the request-body bound this host actually implements,
// `maxHTTPTransportBody` (http_transport.go:28, checked at :87) — a
// property of Orion's transport, observed exactly like `method`'s
// normalization is observed without Blue mandating a verb allowlist.
// Deliberately the REQUEST bound, not `maxHTTPTransportResponse` (:121,
// response body): the fixture describes the request's `body`, and asserting
// the wrong one of Orion's two caps would silently prove a different
// property than the one this test names — exactly the class of defect this
// chantier exists to close. Transmission fidelity — an accepted body
// reaches the wire unmodified — is proven alongside.
func TestHTTPTransportObservesURLTimeoutAndBodyOnWire(t *testing.T) {
	t.Run("authored path and query reach the wire unmodified", func(t *testing.T) {
		type observation struct {
			path  string
			query string
		}
		received := make(chan observation, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			received <- observation{path: r.URL.Path, query: r.URL.RawQuery}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		serverURL, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		egress := effects.NewEgressPolicy([]string{serverURL.Hostname()}, true).InsecureAllowPrivateForTest()
		request := map[string]any{"url": server.URL + "/contract-fixture-path?from=t3", "method": "GET"}

		if _, err := doHTTPRequest(context.Background(), egress, request); err != nil {
			t.Fatalf("transport rejected a well-formed authored url: %v", err)
		}
		select {
		case got := <-received:
			if got.path != "/contract-fixture-path" {
				t.Fatalf("authored path did not reach the wire: got %q", got.path)
			}
			if got.query != "from=t3" {
				t.Fatalf("authored query did not reach the wire: got %q", got.query)
			}
		default:
			t.Fatal("server never received a request")
		}
	})

	t.Run("timeout_ms is enforced against the real connection, not recomputed", func(t *testing.T) {
		unblock := make(chan struct{})
		serverDone := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			<-unblock // holds the connection open past the authored timeout
		}))
		defer func() {
			close(unblock)
			server.Close()
			<-serverDone
		}()

		serverURL, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		egress := effects.NewEgressPolicy([]string{serverURL.Hostname()}, true).InsecureAllowPrivateForTest()
		request := map[string]any{"url": server.URL, "method": "GET", "timeout_ms": json.Number("50")}

		start := time.Now()
		_, err = doHTTPRequest(context.Background(), egress, request)
		elapsed := time.Since(start)
		close(serverDone)
		if err == nil {
			t.Fatal("expected the 50ms authored timeout to abort a request the server never answers")
		}
		if elapsed > 5*time.Second {
			t.Fatalf("transport took %s to fail — timeout_ms was not enforced against the real wire", elapsed)
		}
	})

	t.Run("body reaches the wire byte-identical to what was authored", func(t *testing.T) {
		received := make(chan []byte, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			received <- body
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		serverURL, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		egress := effects.NewEgressPolicy([]string{serverURL.Hostname()}, true).InsecureAllowPrivateForTest()
		// Authored as a string: bodyBytes() passes a string body through
		// verbatim (effects.go), so this is the one shape whose wire bytes
		// are exactly, not just semantically, what was authored.
		authoredBody := `{"contract":"core.http.request@1","n":1}`
		request := map[string]any{"url": server.URL, "method": "POST", "body": authoredBody}

		if _, err := doHTTPRequest(context.Background(), egress, request); err != nil {
			t.Fatalf("transport rejected a well-formed authored body: %v", err)
		}
		select {
		case got := <-received:
			if !bytes.Equal(got, []byte(authoredBody)) {
				t.Fatalf("body did not reach the wire byte-identical: got %q want %q", got, authoredBody)
			}
		default:
			t.Fatal("server never received a request")
		}
	})

	t.Run("Orion's own request-body bound is enforced on the wire, never Blue's", func(t *testing.T) {
		received := make(chan struct{}, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			received <- struct{}{}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		serverURL, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		egress := effects.NewEgressPolicy([]string{serverURL.Hostname()}, true).InsecureAllowPrivateForTest()

		// At the bound: maxHTTPTransportBody is the REQUEST cap
		// (http_transport.go:28,87) — the fixture's `body` field, never
		// maxHTTPTransportResponse (:121), which bounds the response instead
		// and would prove a different property than the one this test names.
		atBound := strings.Repeat("x", maxHTTPTransportBody)
		if _, err := doHTTPRequest(context.Background(), egress, map[string]any{"url": server.URL, "method": "POST", "body": atBound}); err != nil {
			t.Fatalf("a request body exactly at Orion's own bound (%d bytes) was refused: %v", maxHTTPTransportBody, err)
		}
		select {
		case <-received:
		default:
			t.Fatal("at-bound body: server never received a request")
		}

		// Over the bound: refused, and the refusal happens before dispatch —
		// the server must never see a request at all.
		overBound := strings.Repeat("x", maxHTTPTransportBody+1)
		_, err = doHTTPRequest(context.Background(), egress, map[string]any{"url": server.URL, "method": "POST", "body": overBound})
		if err == nil || !strings.Contains(err.Error(), "HTTP_REQUEST_BODY_TOO_LARGE") {
			t.Fatalf("a request body one byte over Orion's own bound (%d bytes) was not refused with HTTP_REQUEST_BODY_TOO_LARGE: %v", maxHTTPTransportBody, err)
		}
		select {
		case <-received:
			t.Fatal("transport refused the over-bound body but the server still received a request — it was emitted anyway")
		default:
		}
	})
}

// --- ADR 013 T3: Orion's contract-fixture arm for core.http.request@1 ------
//
// Four assertions, run in the order §3.4.2 requires because each depends on
// what the previous one authenticated: (a) content — every local copy's
// digest matches the pinned blue-runtime-go table; (b1) the inventory
// extract copy is itself current, not rotated or dropped; (b2) the fixture
// set to hold is derived from that authenticated extract, never declared
// locally; (c) this file's own real path, derived at execution, matches the
// test_path the authenticated extract declares. None of these compares a
// fixture's CONTENT against Blue's model (that is RC2/RC3, and RC3 for
// http_transport.go is covered above) — this arm only proves that Orion's
// copies and this test's location match what Blue's inventory, authenticated
// through the pinned module, currently declares (§3.3.3 "bras livré,
// vérifié").
const (
	// orionContractFixturePath and orionInventoryExtractPath are Orion's own
	// choice of where to keep its byte-identical copies — mirroring Blue's
	// contract-fixtures/ layout exactly is what lets (a) look them up in the
	// table by path. orionInventoryExtractPath's `e3ef100a4e0f` segment is
	// the opaque identifier Orion learned once, in this PR (ADR 013 §3.4):
	// it is the one literal this dispositif does not authenticate — (b1)
	// guards it instead.
	orionContractFixturePath  = "contract-fixtures/core.http.request@1/http_request.v1.json"
	orionInventoryExtractPath = "contract-fixtures/inventory/e3ef100a4e0f.json"

	// orionHTTPContract identifies, within Orion's own (possibly
	// multi-entry, in the future) inventory extract, which entry's
	// test_path assertion (c) below is proving — this file only carries the
	// core.http.request@1 assertion.
	orionHTTPContract = "core.http.request@1"
)

// contractInventoryExtract mirrors the shape Blue publishes at
// contract-fixtures/inventory/<id>.json (blue.s3-contract-inventory-extract.v1).
type contractInventoryExtract struct {
	Entries []struct {
		Contract         string `json:"contract"`
		State            string `json:"state"`
		ConsommeModuleGo bool   `json:"consomme_module_go"`
		TestPath         string `json:"test_path"`
	} `json:"entries"`
}

// contractFixtureModuleRoot walks up from start to the directory holding
// go.mod. It never falls back to a default and never lets a failed walk
// pass silently: a repo laid out unexpectedly is a repo this arm cannot run
// in, and that must fail loudly, not skip.
func contractFixtureModuleRoot(t *testing.T, start string) string {
	t.Helper()
	dir := start
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("contract fixture arm: no go.mod found walking up from %s", start)
	return ""
}

// contractFixtureAssertionPath derives, at execution, this file's own path
// relative to the Orion module root, forward-slash separated — the
// convention the inventory extract's test_path field uses. It is never
// declared as a constant (ADR 013 §3.4.2 (c)): a constant would let this
// file move without the comparison in (c) noticing, which is exactly the
// defect RC7's falsification β exists to catch. The derivation fails loudly
// on any error; it never calls t.Skip (ADR 013 §3.4.2 (c), unlike the
// runtime.Caller precedent at
// internal/compiler/exec_partition_probe_test.go:820-823).
//
// Two build-flag forms are handled because both are real for Orion, not
// hypothetical (ADR 013 §3.4.2, §8): `go test` (no -trimpath, used by every
// CI job) makes runtime.Caller return an absolute filesystem path; `go
// build -trimpath` (Dockerfile:41,48, .github/workflows/win-sidecar.yml:
// 38-39 — what actually builds Orion's production binary) would make it
// return the module path prefixed onto the relative path instead. A
// derivation covering only one form fails bruyamment under the other,
// exactly as ADR 013 §7 T3 requires.
func contractFixtureAssertionPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("contract fixture arm (c): runtime.Caller(0) failed — cannot derive this file's path")
	}
	thisFile = filepath.ToSlash(thisFile)

	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Path == "" {
		t.Fatal("contract fixture arm (c): runtime/debug.ReadBuildInfo() unavailable — cannot derive the module path")
	}

	if trimpathPrefix := info.Main.Path + "/"; strings.HasPrefix(thisFile, trimpathPrefix) {
		// -trimpath form: already module-root-relative.
		return strings.TrimPrefix(thisFile, trimpathPrefix)
	}

	// Absolute-path form: anchor to the go.mod directory by walking up from
	// THIS FILE's own directory — never os.Getwd(), which tracks the
	// invoking directory, not the file carrying the assertion.
	root := contractFixtureModuleRoot(t, filepath.Dir(filepath.FromSlash(thisFile)))
	rel, err := filepath.Rel(root, filepath.FromSlash(thisFile))
	if err != nil {
		t.Fatalf("contract fixture arm (c): cannot make %s relative to module root %s: %v", thisFile, root, err)
	}
	return filepath.ToSlash(rel)
}

// TestContractFixtureLocalCopiesMatchTheirSidecars is ADR 013 §3.4.1's
// self-consistency check: each local copy's digest matches its adjacent
// `.sha256` sidecar. This catches a corrupted or truncated copy — it does
// NOT prove anything about Blue, since the copy and its sidecar travel
// together in the same commit (a compromised or stale copy could carry a
// self-consistent but wrong sidecar). It is deliberately kept separate from
// TestContractFixtureArm_CoreHTTPRequest's (a), which is the real
// authenticity gate against the pinned blue-runtime-go table.
func TestContractFixtureLocalCopiesMatchTheirSidecars(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("contract fixture arm (3.4.1): os.Getwd: %v", err)
	}
	root := contractFixtureModuleRoot(t, wd)

	for _, path := range []string{orionContractFixturePath, orionInventoryExtractPath} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("contract fixture arm (3.4.1): read local copy %s: %v", path, err)
		}
		sidecar, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)+".sha256"))
		if err != nil {
			t.Fatalf("contract fixture arm (3.4.1): read sidecar for %s: %v", path, err)
		}
		sum := sha256.Sum256(data)
		digest := hex.EncodeToString(sum[:])
		wantDigest := strings.Fields(string(sidecar))
		if len(wantDigest) == 0 {
			t.Fatalf("contract fixture arm (3.4.1): sidecar for %s is empty", path)
		}
		if wantDigest[0] != digest {
			t.Fatalf("contract fixture arm (3.4.1): %s does not match its own sidecar — local=%s sidecar=%s (corrupted or truncated copy)", path, digest, wantDigest[0])
		}
	}
}

func TestContractFixtureArm_CoreHTTPRequest(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("contract fixture arm: os.Getwd: %v", err)
	}
	root := contractFixtureModuleRoot(t, wd)

	tableByPath := make(map[string]contractfixtures.Entry, len(contractfixtures.Table))
	for _, entry := range contractfixtures.Table {
		tableByPath[entry.Path] = entry
	}

	// (a) — every local copy's digest matches the table entry of the same
	// path. Nothing below this point is allowed to read the extract before
	// this passes: (b1)/(b2)/(c) all rely on what (a) just authenticated.
	for _, path := range []string{orionContractFixturePath, orionInventoryExtractPath} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("contract fixture arm (a): read local copy %s: %v", path, err)
		}
		sum := sha256.Sum256(data)
		digest := hex.EncodeToString(sum[:])
		entry, found := tableByPath[path]
		if !found {
			t.Fatalf("contract fixture arm (a): %s has no entry in the pinned blue-runtime-go table", path)
		}
		if entry.SHA256 != digest {
			t.Fatalf("contract fixture arm (a): %s digest drifted — local=%s table=%s (bump the pin and resynchronise the copy)", path, digest, entry.SHA256)
		}
	}

	// (b1) — the extract copy itself is current, not rotated or dropped.
	extractEntry := tableByPath[orionInventoryExtractPath]
	if extractEntry.State != contractfixtures.StateCurrent {
		t.Fatalf("contract fixture arm (b1): local inventory extract %s is state=%q, not current — Blue rotated it; read CHANGELOG.md for the new path (ADR 013 §3.4.2 residu i)", orionInventoryExtractPath, extractEntry.State)
	}

	// (a) just authenticated the extract's bytes — parse it now, never
	// before.
	extractData, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(orionInventoryExtractPath)))
	if err != nil {
		t.Fatalf("contract fixture arm: re-read authenticated extract: %v", err)
	}
	var extract contractInventoryExtract
	if err := json.Unmarshal(extractData, &extract); err != nil {
		t.Fatalf("contract fixture arm: decode authenticated extract: %v", err)
	}

	// (b2) — the fixture set to hold is DERIVED from the authenticated
	// extract, never declared locally. Any current table entry under
	// contract-fixtures/<contract>/ for a contract this extract lists, with
	// no matching local copy on disk, fails.
	contracts := make(map[string]struct{}, len(extract.Entries))
	for _, e := range extract.Entries {
		contracts[e.Contract] = struct{}{}
	}
	for _, entry := range contractfixtures.Table {
		if entry.State != contractfixtures.StateCurrent {
			continue
		}
		for contract := range contracts {
			if !strings.HasPrefix(entry.Path, "contract-fixtures/"+contract+"/") {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(entry.Path))); err != nil {
				t.Errorf("contract fixture arm (b2): extract declares contract %q current at %s but no local copy exists", contract, entry.Path)
			}
		}
	}

	// (c) — this file's own real path, derived at execution, equals the
	// test_path the authenticated extract declares for this contract.
	wantPath, found := "", false
	for _, e := range extract.Entries {
		if e.Contract == orionHTTPContract {
			wantPath, found = e.TestPath, true
			break
		}
	}
	if !found {
		t.Fatalf("contract fixture arm (c): authenticated extract has no entry for %q", orionHTTPContract)
	}
	if gotPath := contractFixtureAssertionPath(t); gotPath != wantPath {
		t.Fatalf("contract fixture arm (c): this file's real path %q does not match the inventory's declared test_path %q — either Blue's inventory is stale or this file moved without a matching Blue PR (ADR 013 §3.9 G8)", gotPath, wantPath)
	}
}
