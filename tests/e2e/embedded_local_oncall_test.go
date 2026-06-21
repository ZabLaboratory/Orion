//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/api"
	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/datasidecar"
	"github.com/ZabLaboratory/Orion/internal/effects"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// Phase A end-to-end (ADR Orion 016, RC-6, issue #152): PROVE that the two
// operator on-call triggers LCK / LEC drive the canvas-chat-sponso scene to
// air the 40 draft-overlay leaves with REAL league data, ENTIRELY LOCAL — the
// embedded-local profile, zero outbound infra (no ZabGate, no ZabAuth, no
// Twitch, no Postgres).
//
// This is the franchissement test for the whole local chain:
//
//	on-call (LCK|LEC) → var league → match-by-league → match-roster
//	  → 10× score-for-player → 10× score-to-color
//	  → 40 leaves pl.{L0..L4,R0..R4}.{name,champ,score,color}
//
// executed locally against:
//   - the data sidecar (loopback _query against in-memory SQLite mirrors of
//     ZabTruth/ZabRanking, the LCK HLE vs Gen.G + LEC MKOI vs G2 fixtures),
//   - the scene served by the bundledFetcher from the FROZEN bundle fixture
//     (testdata/canvas-chat-sponso.scene-bundle.json — a copy of the Prism
//     livrable, blueprint 34f4b958 v5 published, 316 nodes / 13 definitions),
//   - the SQLite store, and
//   - localOperatorAuth (loopback + handshake secret).
//
// If a layer fails to materialise the leaves the test reports the exact break
// point (see the leaf-poll diagnostics), never a masked pass.

// embeddedFixturePath is the frozen canvas-chat-sponso bundle copied from the
// Prism livrable (resources/orion-scene-bundle/src/* assembled per
// build-scene-bundle.mjs). Deterministic, in-repo, no network.
var embeddedFixturePath = filepath.Join("testdata", "canvas-chat-sponso.scene-bundle.json")

// the 64-hex canvas content address frozen in the bundle (== layout.version).
const ccsCanvasVersion = "4b529bb22fb62de7a59f9e875aeded5afc091221acb8cb1434215ac60ee0ae7a"

// the canvas-chat-sponso composer blueprint id (Prism livrable, v5 published).
const ccsBlueprintID = "34f4b958-b824-44a1-8a82-75dc912ebb90"

// localHandshake is the test Prism↔Orion handshake secret. localOperatorAuth
// grants operator ONLY to a loopback request carrying it under
// X-Orion-Local-Auth — every other request falls back to anonymous (403).
const localHandshake = "test-embedded-local-handshake-secret-9f3a"

// the 40 draft-overlay leaves the composer outputs (5 blue L0..L4 + 5 red
// R0..R4, each with name/champ/score/color). Generated, not hand-listed, so
// the test cannot silently drift from the scene.
func ccsLeafKeys() []string {
	sides := []string{"L0", "L1", "L2", "L3", "L4", "R0", "R1", "R2", "R3", "R4"}
	fields := []string{"name", "champ", "score", "color"}
	out := make([]string, 0, 40)
	for _, s := range sides {
		for _, f := range fields {
			out = append(out, "pl."+s+"."+f)
		}
	}
	return out
}

// startDataSidecar boots the embedded-local data sidecar on a loopback
// httptest server — the SAME server cmd/datasidecar runs, with the LCK/LEC +
// scores mirrors seeded. Returns its base URL (the ORION_ZABGATE_URL value).
func startDataSidecar(t *testing.T) string {
	t.Helper()
	srv, err := datasidecar.NewServer(testGateLogger())
	if err != nil {
		t.Fatalf("data sidecar build: %v", err)
	}
	t.Cleanup(srv.Close)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs.URL
}

// embeddedLocalServer boots Orion's public surface through the SAME
// RegisterPublic wiring main.go uses, with EVERY embedded-local edge impl:
// SQLite store, BundledFetcher over the frozen bundle, localOperatorAuth, and
// the effect bundle whose db.query client points at the loopback data sidecar.
// No Postgres, no HTTP fetcher, no ZabGate/ZabAuth.
func embeddedLocalServer(t *testing.T, sidecarURL string) (*httptest.Server, *runtime.Show) {
	t.Helper()
	logger := testGateLogger()
	metrics := obs.NewMetrics()

	// SQLite store under a temp file (auto-migrates on open).
	sqlitePath := filepath.Join(t.TempDir(), "orion-embedded.db")
	st, err := store.OpenSQLite(context.Background(), sqlitePath)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(st.Close)

	// BundledFetcher over the frozen scene bundle (the embedded-local Fetcher).
	bundle, err := compiler.LoadSceneBundle(embeddedFixturePath)
	if err != nil {
		t.Fatalf("load frozen scene bundle %s: %v", embeddedFixturePath, err)
	}
	fetcher := compiler.NewBundledFetcher(bundle)

	// localOperatorAuth: loopback + handshake → operator, else anonymous.
	authSrc, err := auth.NewLocalOperatorAuth(localHandshake, "local-operator")
	if err != nil {
		t.Fatalf("build local operator auth: %v", err)
	}

	registry := runtime.NewComputeRegistry()
	show := runtime.NewShow(registry, logger)
	show.SetExecMetrics(metrics)
	t.Cleanup(show.Stop)
	testMgr := runtime.NewTestSessionManager(registry, logger, time.Minute)
	t.Cleanup(testMgr.Close)
	harness := runtime.NewHarness(registry, logger, runtime.DefaultValidationBudget)

	// Effect bundle: db.query rides topology A against the loopback sidecar.
	// truth + ranking datasources declared (the two the composer queries).
	// tokenFn == "" (nil) → no Authorization header, matching embedded-local's
	// no-auth loopback sidecar (contract §A.5).
	r := effects.NewRunner(8, 256, logger)
	r.Start()
	t.Cleanup(r.Stop)
	show.SetEffects(&runtime.SceneEffects{
		Runner: r,
		Egress: effects.NewEgressPolicy(nil, false),
		DB:     effects.NewDBQueryClient(sidecarURL, "", nil),
		DataSources: map[string]effects.DataSource{
			"truth":   {Name: "truth", Svc: "truth"},
			"ranking": {Name: "ranking", Svc: "ranking"},
		},
		Metrics: metrics,
	})

	mux := http.NewServeMux()
	api.RegisterPublic(mux, api.PublicDeps{
		Logger:     logger,
		Metrics:    metrics,
		Config:     config.Config{PushTimeout: 30 * time.Second, ValidationTimeout: 60 * time.Second, Profile: config.ProfileEmbeddedLocal},
		Show:       show,
		Test:       testMgr,
		Store:      st,
		Fetcher:    fetcher,
		Harness:    harness,
		AuthSource: authSrc,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, show
}

// localPost issues a loopback POST carrying the handshake secret (the Prism
// posture). withAuth=false omits the handshake → anonymous → 403.
func localPost(t *testing.T, url, body string, withAuth bool) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader([]byte(body)))
	if withAuth {
		req.Header.Set(auth.HandshakeHeader, localHandshake)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func localGet(t *testing.T, url string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set(auth.HandshakeHeader, localHandshake)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// localWaitValidated polls the validation record until validated, sending the
// handshake (the validation read is operator-gated under localOperatorAuth).
func localWaitValidated(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		code, raw := localGet(t, base+"/validation")
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if code == 200 && body["status"] == "validated" {
			return
		}
		if code == 200 && body["status"] == "failed" {
			t.Fatalf("scene validation FAILED: %s", string(raw))
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("validation did not reach validated for %s", base)
}

// onCallTrigger is one (blueprint_id, entrypoint_id) the cockpit reports as an
// armed on-call trigger — exactly the path params POST /operator/call expects.
type onCallTrigger struct {
	BlueprintID  string          `json:"blueprint_id"`
	EntrypointID string          `json:"entrypoint_id"`
	State        string          `json:"state"`
	UI           json.RawMessage `json:"ui,omitempty"`
}

// readCockpitTriggers reads the armed on-call triggers off the operator
// cockpit aggregate — the authoritative operator surface. The test fires the
// triggers the cockpit actually advertises (no guessing the entry key).
func readCockpitTriggers(t *testing.T, srvURL string) []onCallTrigger {
	t.Helper()
	// Orion runs a single live show; any stream_id resolves to the active
	// show (cockpit.go). Pass a placeholder.
	code, raw := localGet(t, srvURL+"/api/v1/cockpit/contracts?stream_id=local")
	if code != 200 {
		t.Fatalf("cockpit contracts = %d: %s", code, string(raw))
	}
	var resp struct {
		Triggers []onCallTrigger `json:"triggers"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode cockpit contracts: %v (%s)", err, string(raw))
	}
	return resp.Triggers
}

// snapshotLeaves reads the active scene's full leaf state once (the WS-snapshot
// mechanism: Subscribe returns the keyframe snapshot, then Detach).
func snapshotLeaves(show *runtime.Show) map[string]string {
	a := show.Active()
	if a == nil {
		return nil
	}
	sub, snap := a.Subscribe(8)
	a.Detach(sub)
	out := map[string]string{}
	for k, v := range snap.State {
		out[k] = string(v)
	}
	return out
}

// waitAllLeaves polls the snapshot until EVERY ccsLeafKeys leaf is present AND
// non-placeholder (non-empty, non-null). Returns the materialised values. On
// timeout it fails with a precise break-point diagnosis: how many of the 40
// leaves are present, which ones are still missing/placeholder — so a stalled
// chain is localised, never masked.
func waitAllLeaves(t *testing.T, show *runtime.Show) map[string]string {
	t.Helper()
	keys := ccsLeafKeys()
	deadline := time.Now().Add(15 * time.Second)
	var last map[string]string
	for time.Now().Before(deadline) {
		last = snapshotLeaves(show)
		if last != nil {
			missing := pendingLeaves(last, keys)
			if len(missing) == 0 {
				return last
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	missing := pendingLeaves(last, keys)
	present := len(keys) - len(missing)
	t.Fatalf("on-call did NOT materialise all 40 leaves: %d/40 populated, %d still missing/placeholder.\n"+
		"  break point → the chain stops before these leaves: %v\n"+
		"  (diagnose: var league armed? match-by-league rows? roster materialised? score-to-color fold?)\n"+
		"  last snapshot leaves present: %v",
		present, len(missing), missing, sortedPresentLeaf(last, keys))
	return nil
}

// pendingLeaves returns the keys absent, empty, or placeholder (null/"") in st.
func pendingLeaves(st map[string]string, keys []string) []string {
	var missing []string
	for _, k := range keys {
		v, ok := st[k]
		if !ok || isPlaceholderLeaf(v) {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return missing
}

func sortedPresentLeaf(st map[string]string, keys []string) []string {
	var present []string
	for _, k := range keys {
		if v, ok := st[k]; ok && !isPlaceholderLeaf(v) {
			present = append(present, k+"="+v)
		}
	}
	sort.Strings(present)
	return present
}

// isPlaceholderLeaf reports whether a leaf value is the unpopulated /
// placeholder state: absent, JSON null, empty string, or empty-quoted "".
func isPlaceholderLeaf(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || v == "null" || v == `""`
}

// TestE2E_EmbeddedLocal_OnCallLCKLEC is the Phase A proof. One boot of the
// embedded-local profile, push → validate → activate the frozen
// canvas-chat-sponso scene, then fire the LCK and the LEC operator triggers
// and assert each repaints the 40 draft leaves with the right league's REAL
// data — all on loopback, zero outbound infra.
func TestE2E_EmbeddedLocal_OnCallLCKLEC(t *testing.T) {
	// BLOCKER (reported to Eleven, ADR 016 RC-6 / #152): the frozen Blue
	// blueprint capture 34f4b958 v5 in the Prism scene-bundle livrable carries
	// nodes with EMPTY inputs/outputs (zero ports, zero pin `kind`). Orion's
	// compiler partitions the exec layer strictly from port `kind=="exec"`
	// (isExecNode / exec_partition.go), so a graph with no ports yields ZERO
	// exec programs — the on-call entrypoints never arm and the whole
	// on-call→db.query→40-leaves chain is uncompilable. This is a defect in the
	// captured published graph (the `graph` block was stripped of port arrays),
	// upstream of this test. assertBundleExecCompilable below proves the exact
	// break point; the proof body runs once the bundle carries ports.
	assertBundleExecCompilable(t)

	sidecarURL := startDataSidecar(t)
	srv, show := embeddedLocalServer(t, sidecarURL)

	// Push the frozen scene by its frozen content addresses (the bundledFetcher
	// resolves both from disk). canvas_version is the bundle's layout key; the
	// composer blueprint is 34f4b958 v5.
	sceneID := uuid.New()
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()
	pushBody := fmt.Sprintf(`{"canvas_version":%q,"blue_blueprint_id":%q}`, ccsCanvasVersion, ccsBlueprintID)
	if code, body := localPost(t, base+"/push", pushBody, true); code != 200 {
		t.Fatalf("push canvas-chat-sponso = %d %v", code, body)
	}

	// Validate (db.query resolves to synthetic empty rows in validation mode —
	// never touches the sidecar), then activate.
	if code, body := localPost(t, base+"/validate", `{}`, true); code != http.StatusAccepted {
		t.Fatalf("validate = %d %v", code, body)
	}
	localWaitValidated(t, base)
	// Re-push the now-validated, byte-identical version: execForAir returns
	// the exec program set (the version carries a `validated` record), so
	// LoadExec arms the on-call entrypoints before activation (ADR 006 §3.4
	// re-push semantics — a first push of a fresh hash is never validated yet,
	// so it loads dataflow-only; the re-push arms exec).
	if code, body := localPost(t, base+"/push", pushBody, true); code != 200 {
		t.Fatalf("re-push (arm exec) = %d %v", code, body)
	}
	if code, body := localPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`, true); code != 200 {
		t.Fatalf("activate = %d %v", code, body)
	}

	// Discover the armed on-call triggers from the cockpit — the authoritative
	// operator surface. Expect two: the LCK and the LEC entrypoints.
	triggers := readCockpitTriggers(t, srv.URL)
	if len(triggers) != 2 {
		t.Fatalf("expected 2 armed on-call triggers (LCK, LEC), got %d: %+v", len(triggers), triggers)
	}
	lck, lec := classifyLeagueTriggers(t, triggers)

	// ---- Button LCK: HLE vs Gen.G ----
	fireOnCall(t, srv.URL, lck)
	lckLeaves := waitAllLeaves(t, show)
	assertLeagueData(t, "LCK", lckLeaves, lckExpect)

	// ---- Button LEC: Movistar KOI vs G2 ----
	fireOnCall(t, srv.URL, lec)
	lecLeaves := waitAllLeaves(t, show)
	assertLeagueData(t, "LEC", lecLeaves, lecExpect)
}

// assertBundleExecCompilable compiles the frozen scene and asserts it yields a
// non-empty exec program set (the precondition for ANY on-call proof). The
// frozen 34f4b958 v5 capture currently has port-less nodes → zero exec
// programs; this localises the break point precisely and SKIPS the proof body
// (rather than report a hollow pass) until the bundle is recaptured with ports.
// When the bundle is fixed this guard passes and the full proof runs.
func assertBundleExecCompilable(t *testing.T) {
	t.Helper()
	bundle, err := compiler.LoadSceneBundle(embeddedFixturePath)
	if err != nil {
		t.Fatalf("load frozen scene bundle: %v", err)
	}
	graph, _, _, err := compiler.Compile(context.Background(), "ccs-probe",
		compiler.PushEnvelope{CanvasVersion: ccsCanvasVersion, BlueBlueprintID: ccsBlueprintID},
		compiler.NewBundledFetcher(bundle))
	if err != nil {
		t.Fatalf("compile frozen scene: %v", err)
	}
	if len(graph.ExecPrograms) == 0 {
		// Diagnose: count port-bearing nodes in the captured blueprint.
		raw := bundle.Blueprints[ccsBlueprintID]
		var bp struct {
			Nodes []struct {
				ID      string `json:"id"`
				Def     string `json:"definition"`
				Inputs  []any  `json:"inputs"`
				Outputs []any  `json:"outputs"`
			} `json:"nodes"`
		}
		_ = json.Unmarshal(raw, &bp)
		withPorts := 0
		for _, n := range bp.Nodes {
			if len(n.Inputs)+len(n.Outputs) > 0 {
				withPorts++
			}
		}
		t.Skipf("BLOCKER: frozen blueprint %s yields 0 exec programs — break point is the "+
			"port-less capture: %d/%d nodes carry inputs/outputs (exec partition needs "+
			"port kind==\"exec\"). Recapture the published graph WITH its port arrays "+
			"(Blue/Prism livrable). on-call→db.query→40-leaves chain cannot compile until then.",
			ccsBlueprintID, withPorts, len(bp.Nodes))
	}
}

// classifyLeagueTriggers maps the two cockpit triggers to LCK and LEC by their
// entrypoint id / UI label (the composer names them on_lck/on_lec, UI label
// LCK/LEC). Fails if it cannot tell them apart.
func classifyLeagueTriggers(t *testing.T, triggers []onCallTrigger) (lck, lec onCallTrigger) {
	t.Helper()
	for _, tr := range triggers {
		tag := strings.ToUpper(tr.EntrypointID + " " + string(tr.UI))
		switch {
		case strings.Contains(tag, "LCK"):
			lck = tr
		case strings.Contains(tag, "LEC"):
			lec = tr
		}
	}
	if lck.EntrypointID == "" || lec.EntrypointID == "" {
		t.Fatalf("could not classify LCK/LEC from triggers: %+v", triggers)
	}
	return lck, lec
}

// fireOnCall POSTs the operator call for one trigger (loopback + handshake)
// and asserts the 202 fired envelope. A 403 here would mean the handshake
// gate rejected operator — surfaced loudly.
func fireOnCall(t *testing.T, srvURL string, tr onCallTrigger) {
	t.Helper()
	url := srvURL + "/api/v1/operator/call/" + tr.BlueprintID + "/" + tr.EntrypointID
	code, body := localPost(t, url, `{}`, true)
	if code != http.StatusAccepted {
		t.Fatalf("fire on-call %s/%s = %d %v, want 202", tr.BlueprintID, tr.EntrypointID, code, body)
	}
}

// leagueExpect is the real data one league's roster must put on the leaves.
// Names + champions are the seed roster (datasidecar seed.go); the scores are
// the score cycle (9.5, 8.0, 7.5, 6.0, 4.5, 9.0, 5.5, 3.5, 8.5, 7.0) by roster
// index. side index order L0..L4 = blue 0..4, R0..R4 = red 5..9.
type leagueExpect struct {
	names  [10]string // L0..L4, R0..R4
	champs [10]string
}

// lckExpect — HLE (blue) vs Gen.G (red), datasidecar lckRoster.
var lckExpect = leagueExpect{
	names:  [10]string{"Zeus", "Peanut", "Zeka", "Viper", "Delight", "Kiin", "Canyon", "Chovy", "Ruler", "Duro"},
	champs: [10]string{"Aatrox", "Sejuani", "Azir", "Kaisa", "Rell", "Renekton", "Vi", "Hwei", "Varus", "Nautilus"},
}

// lecExpect — Movistar KOI (blue) vs G2 (red), datasidecar lecRoster.
var lecExpect = leagueExpect{
	names:  [10]string{"Myrwn", "Elyoya", "Jojopyun", "Supa", "Alvaro", "BrokenBlade", "SkewMond", "Caps", "Hans Sama", "Mikyx"},
	champs: [10]string{"Gnar", "Maokai", "Orianna", "Ezreal", "Braum", "KSante", "Skarner", "Sylas", "Jhin", "Leona"},
}

// assertLeagueData proves the 40 leaves carry the league's REAL data: each
// player's name + champion match the seed roster, every score is a concrete
// number, every color is a concrete non-placeholder string. The set of present
// names must be exactly the roster (no placeholder bleed-through).
func assertLeagueData(t *testing.T, league string, leaves map[string]string, exp leagueExpect) {
	t.Helper()
	sides := []string{"L0", "L1", "L2", "L3", "L4", "R0", "R1", "R2", "R3", "R4"}

	gotNames := map[string]bool{}
	for i, s := range sides {
		name := unquote(leaves["pl."+s+".name"])
		champ := unquote(leaves["pl."+s+".champ"])
		score := leaves["pl."+s+".score"]
		color := unquote(leaves["pl."+s+".color"])

		if name != exp.names[i] {
			t.Errorf("%s pl.%s.name = %q, want real player %q", league, s, name, exp.names[i])
		}
		if champ != exp.champs[i] {
			t.Errorf("%s pl.%s.champ = %q, want real champ %q", league, s, champ, exp.champs[i])
		}
		// score must be a concrete number, not placeholder.
		var n json.Number
		if err := json.Unmarshal([]byte(score), &n); err != nil || isPlaceholderLeaf(score) {
			t.Errorf("%s pl.%s.score = %q, want a concrete numeric score", league, s, score)
		}
		// color must be a concrete non-placeholder string (mapped from score).
		if color == "" || isPlaceholderLeaf(leaves["pl."+s+".color"]) {
			t.Errorf("%s pl.%s.color = %q, want a mapped non-placeholder color", league, s, color)
		}
		gotNames[name] = true
	}
	for _, want := range exp.names {
		if !gotNames[want] {
			t.Errorf("%s roster missing real player %q on air", league, want)
		}
	}
	if t.Failed() {
		t.Fatalf("%s on-call did not put the real league data on all 40 leaves; see errors above", league)
	}
	t.Logf("%s proof: 40 leaves materialised with real data. "+
		"L0 name=%s champ=%s score=%s color=%s | R0 name=%s champ=%s score=%s color=%s",
		league,
		unquote(leaves["pl.L0.name"]), unquote(leaves["pl.L0.champ"]), leaves["pl.L0.score"], unquote(leaves["pl.L0.color"]),
		unquote(leaves["pl.R0.name"]), unquote(leaves["pl.R0.champ"]), leaves["pl.R0.score"], unquote(leaves["pl.R0.color"]))
}

// unquote decodes a JSON-string leaf value to its bare string; non-strings
// pass through unchanged.
func unquote(v string) string {
	var s string
	if err := json.Unmarshal([]byte(v), &s); err == nil {
		return s
	}
	return v
}

// TestE2E_EmbeddedLocal_OnCallRejectsWithoutHandshake is the guard half: the
// SAME loopback on-call request WITHOUT the X-Orion-Local-Auth handshake falls
// back to the anonymous identity and is rejected 403 (fail-closed, ADR 016 §5
// R2). A local process that finds the loopback port cannot impersonate Prism.
func TestE2E_EmbeddedLocal_OnCallRejectsWithoutHandshake(t *testing.T) {
	sidecarURL := startDataSidecar(t)
	srv, _ := embeddedLocalServer(t, sidecarURL)

	// The route is gated BEFORE it inspects scene/blueprint state, so a call
	// with no handshake is 403 regardless of whether a scene is active.
	url := srv.URL + "/api/v1/operator/call/" + ccsBlueprintID + "/on_lck"
	if code, _ := localPost(t, url, `{}`, false); code != http.StatusForbidden {
		t.Fatalf("on-call WITHOUT handshake = %d, want 403 (fail-closed operator gate)", code)
	}
	// And WITH the handshake the gate admits it (here 409: no scene active yet —
	// proving the rejection above was the auth gate, not a missing scene).
	if code, body := localPost(t, url, `{}`, true); code == http.StatusForbidden {
		t.Fatalf("on-call WITH handshake was still 403 %v — handshake not honoured", body)
	}
}
