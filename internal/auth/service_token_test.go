package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/secretbox"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// ---------------------------------------------------------------- test doubles

// fakeTokenStore is the encrypted-column surface #303 delivered, in memory. It
// holds opaque bytes exactly as Postgres does, and can be made to fail a chosen
// suffix of writes so the persist-before-swap failure branch is exercised for
// real rather than described in a comment (RC 43 / RC 50).
type fakeTokenStore struct {
	mu   sync.Mutex
	blob []byte
	has  bool
	// failAfter > 0 ⇒ every write past the failAfter-th fails. 0 ⇒ all succeed.
	failAfter int
	writes    int
	reads     int
}

func (f *fakeTokenStore) GetServiceRefreshToken(context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if !f.has {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), f.blob...), nil
}

func (f *fakeTokenStore) PutServiceRefreshToken(_ context.Context, enc []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.failAfter > 0 && f.writes > f.failAfter {
		return errors.New("fake store: write refused")
	}
	f.blob = append([]byte(nil), enc...)
	f.has = true
	return nil
}

func (f *fakeTokenStore) heal() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAfter = 0
}

func (f *fakeTokenStore) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

// record decrypts what the store currently holds.
func (f *fakeTokenStore) record(t *testing.T, box *secretbox.Box) durableRecord {
	t.Helper()
	f.mu.Lock()
	blob := append([]byte(nil), f.blob...)
	has := f.has
	f.mu.Unlock()
	if !has {
		t.Fatal("store holds nothing")
	}
	plain, err := box.Open(blob)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var rec durableRecord
	if err := json.Unmarshal(plain, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return rec
}

// zabAuthStub answers /service-tokens/refresh and counts everything. A call to
// the MINT route is a test failure by construction: under the durable model
// Orion has no credential to mint with and must never try.
type zabAuthStub struct {
	srv           *httptest.Server
	refreshCalls  atomic.Int32
	mintCalls     atomic.Int32
	lastPresented atomic.Value // string
	// status, when non-zero, is the answer to the next refresh.
	status atomic.Int32
	gen    atomic.Int32
	ttl    time.Duration
}

func newZabAuthStub(t *testing.T) *zabAuthStub {
	t.Helper()
	s := &zabAuthStub{ttl: time.Hour}
	s.lastPresented.Store("")
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/api/v1/service-tokens/refresh":
			s.refreshCalls.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.lastPresented.Store(body["refresh_token"])
			if code := s.status.Load(); code != 0 {
				http.Error(w, `{"detail":"refresh token reuse detected"}`, int(code))
				return
			}
			n := s.gen.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":       "access-g" + itoa(n),
				"refresh_token":      "refresh-g" + itoa(n),
				"expires_at":         time.Now().Add(s.ttl).Format(time.RFC3339Nano),
				"refresh_expires_at": time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339Nano),
			})
		case "/auth/api/v1/service-tokens":
			s.mintCalls.Add(1)
			http.Error(w, "mint is forbidden under the durable model", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *zabAuthStub) refreshURL() string {
	return s.srv.URL + "/auth/api/v1/service-tokens/refresh"
}

func (s *zabAuthStub) presented() string { return s.lastPresented.Load().(string) }

func itoa(n int32) string { return strconv.Itoa(int(n)) }

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	raw := make([]byte, secretbox.KeyBytes)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

func durableManager(stub *zabAuthStub, st *fakeTokenStore, box *secretbox.Box, seed string) *ServiceTokenManager {
	return &ServiceTokenManager{
		RefreshURL:      stub.refreshURL(),
		Seed:            seed,
		Store:           st,
		Box:             box,
		TransportRetry:  time.Hour, // no background noise inside a unit test
		PersistRetryMin: 20 * time.Millisecond,
		PersistRetryMax: 60 * time.Millisecond,
	}
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ------------------------------------------------------------------- the tests

// Static mode is the dev/test posture: no store, no box, no ZabAuth round-trip.
func TestServiceTokenManager_StaticMode(t *testing.T) {
	m := &ServiceTokenManager{StaticToken: "static-abc"}
	if got := m.Token(); got != "static-abc" {
		t.Fatalf("static mode token = %q, want static-abc", got)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start in static mode should be a no-op, got err=%v", err)
	}
	if got := m.Token(); got != "static-abc" {
		t.Fatalf("token after Start (static mode) = %q, want static-abc", got)
	}
	if got := m.State(); got != ServiceTokenArmed {
		t.Fatalf("state = %q, want armed", got)
	}
	m.Stop() // safe in static mode
}

// The boot gesture is a REFRESH, never a mint (§ A3.3 part 1): Orion holds no
// access token at rest, so Start() rotates the seed and adopts the family
// (generation 0 → 1) on the first boot. A second boot resolves the ROTATED
// value from the store and does not replay the seed (RC 41, second half).
func TestServiceTokenManager_BootRefreshesNeverMints_AndSecondBootDoesNotReplaySeed(t *testing.T) {
	stub := newZabAuthStub(t)
	st := &fakeTokenStore{}
	box := testBox(t)

	m := durableManager(stub, st, box, "seed-gen0")
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := stub.mintCalls.Load(); got != 0 {
		t.Fatalf("mint calls = %d, want 0 — the durable model never mints", got)
	}
	if got := stub.refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if got := stub.presented(); got != "seed-gen0" {
		t.Fatalf("first boot presented %q, want the seed", got)
	}
	if got := m.Token(); got != "access-g1" {
		t.Fatalf("token = %q, want access-g1", got)
	}
	if got := m.State(); got != ServiceTokenArmed {
		t.Fatalf("state = %q, want armed", got)
	}
	rec := st.record(t, box)
	if rec.RefreshToken != "refresh-g1" || rec.Rotating {
		t.Fatalf("persisted record = %+v, want the rotated value, not rotating", rec)
	}
	m.Stop()

	// Second boot: same store, seed still present in the environment.
	m2 := durableManager(stub, st, box, "seed-gen0")
	if err := m2.Start(context.Background()); err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	defer m2.Stop()
	if got := stub.presented(); got != "refresh-g1" {
		t.Fatalf("second boot presented %q, want the rotated value (replaying the seed would trip reuse)", got)
	}
	if got := m2.Token(); got != "access-g2" {
		t.Fatalf("token after second boot = %q, want access-g2", got)
	}
	if got := stub.mintCalls.Load(); got != 0 {
		t.Fatalf("mint calls = %d, want 0", got)
	}
}

// RC 43: a persist failure injected AFTER a successful rotation leaves the
// process serving, marks the state unpersisted — and a restart in that state
// fails closed. "Fails closed" is about the TOKEN, not the process: Start still
// returns nil, the process boots and airs; it simply holds no service token and
// issues no refresh call that would replay a consumed generation.
func TestServiceTokenManager_PersistFailureIsUnpersisted_AndRestartFailsClosed(t *testing.T) {
	stub := newZabAuthStub(t)
	// Write 1 is the rotation marker (succeeds); write 2 is the successor.
	st := &fakeTokenStore{failAfter: 1}
	box := testBox(t)

	m := durableManager(stub, st, box, "seed-gen0")
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := m.Token(); got != "access-g1" {
		t.Fatalf("token = %q — the process must keep serving on the in-memory value", got)
	}
	if got := m.State(); got != ServiceTokenUnpersisted {
		t.Fatalf("state = %q, want unpersisted", got)
	}
	if rec := st.record(t, box); !rec.Rotating || rec.RefreshToken != "seed-gen0" {
		t.Fatalf("store holds %+v, want the pre-rotation value marked rotating", rec)
	}
	m.Stop()

	// Restart against that store. The persisted value is a generation the
	// server has already consumed; replaying it would trip reuse and revoke the
	// family, so the manager refuses.
	refreshesBefore := stub.refreshCalls.Load()
	m2 := durableManager(stub, st, box, "seed-gen0")
	if err := m2.Start(context.Background()); err != nil {
		t.Fatalf("restart must still boot, got err=%v", err)
	}
	defer m2.Stop()
	if got := m2.Token(); got != "" {
		t.Fatalf("token after a fail-closed restart = %q, want empty", got)
	}
	if got := m2.State(); got != ServiceTokenDegraded {
		t.Fatalf("state after a fail-closed restart = %q, want degraded", got)
	}
	if got := stub.refreshCalls.Load(); got != refreshesBefore {
		t.Fatalf("restart issued %d refresh call(s); a fail-closed restart must issue none", got-refreshesBefore)
	}
}

// RC 50 (Bastion-2 R20): the unpersisted state retries the persist on its OWN
// bounded backoff — it does not wait for the next rotation — and the retry
// issues no refresh call. The clock is never advanced towards the next
// rotation here: only the database is made available again.
func TestServiceTokenManager_UnpersistedRetriesOnItsOwnBackoff(t *testing.T) {
	stub := newZabAuthStub(t)
	st := &fakeTokenStore{failAfter: 1}
	box := testBox(t)

	m := durableManager(stub, st, box, "seed-gen0")
	// An hour-long refresh lead would still leave the next rotation ~59 min
	// away; nothing in this test may depend on it.
	m.RefreshLead = time.Minute
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()
	if got := m.State(); got != ServiceTokenUnpersisted {
		t.Fatalf("state = %q, want unpersisted", got)
	}
	refreshesAtFailure := stub.refreshCalls.Load()

	st.heal() // the database answers again — no clock advance, no rotation

	waitFor(t, "the unpersisted flag to clear on its own backoff", 3*time.Second, func() bool {
		return m.State() == ServiceTokenArmed
	})
	if rec := st.record(t, box); rec.RefreshToken != "refresh-g1" || rec.Rotating {
		t.Fatalf("re-persisted record = %+v, want the rotated value, not rotating", rec)
	}
	if got := stub.refreshCalls.Load(); got != refreshesAtFailure {
		t.Fatalf("the persist retry issued %d refresh call(s); it must issue none (it is a single-row write, not a rotation)", got-refreshesAtFailure)
	}
}

// A refresh ZabAuth REJECTS (401 = reuse detected / revoked) is terminal: no
// retry loop on that value, no re-mint — Orion holds no minting credential —
// and the manager goes token-less until an operator re-seeds.
func TestServiceTokenManager_RejectedRefreshIsTerminal(t *testing.T) {
	stub := newZabAuthStub(t)
	stub.status.Store(http.StatusUnauthorized)
	st := &fakeTokenStore{}
	box := testBox(t)

	m := durableManager(stub, st, box, "seed-gen0")
	m.TransportRetry = 10 * time.Millisecond // a transport-class retry would be visible
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("a terminal rejection must not fail the boot, got err=%v", err)
	}
	defer m.Stop()
	if got := m.Token(); got != "" {
		t.Fatalf("token = %q, want empty", got)
	}
	if got := m.State(); got != ServiceTokenDegraded {
		t.Fatalf("state = %q, want degraded", got)
	}
	time.Sleep(150 * time.Millisecond)
	if got := stub.refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1 — a terminal rejection is not retried", got)
	}
	if got := stub.mintCalls.Load(); got != 0 {
		t.Fatalf("mint calls = %d, want 0", got)
	}
}

// 429 is the per-IP failure budget of § 3.7 — "ask again later", not a verdict
// on the value. It must be classified as transport, i.e. retried.
func TestServiceTokenManager_RateLimitIsNotTerminal(t *testing.T) {
	stub := newZabAuthStub(t)
	stub.status.Store(http.StatusTooManyRequests)
	st := &fakeTokenStore{}
	box := testBox(t)

	m := durableManager(stub, st, box, "seed-gen0")
	m.RefreshLead = time.Millisecond
	m.TransportRetry = 10 * time.Millisecond
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()
	stub.status.Store(0) // the budget window elapses
	waitFor(t, "the retried rotation to succeed", 5*time.Second, func() bool {
		return m.Token() != ""
	})
	if got := m.State(); got != ServiceTokenArmed {
		t.Fatalf("state = %q, want armed", got)
	}
}

// RC 48: ZabAuth unreachable at boot. Orion still boots — Start returns nil, so
// run() proceeds and the show airs — and it holds no token rather than falling
// back on any standing credential. StaticToken is set here precisely to prove
// the durable manager ignores it.
func TestServiceTokenManager_ZabAuthUnreachableAtBootStillBoots(t *testing.T) {
	stub := newZabAuthStub(t)
	st := &fakeTokenStore{}
	box := testBox(t)

	m := durableManager(stub, st, box, "seed-gen0")
	m.StaticToken = "static-fallback-must-not-be-used"
	m.RefreshURL = "http://127.0.0.1:1/auth/api/v1/service-tokens/refresh" // closed port
	m.HTTPClient = &http.Client{Timeout: 200 * time.Millisecond}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("an unreachable ZabAuth must not fail the boot, got err=%v", err)
	}
	defer m.Stop()
	if got := m.Token(); got != "" {
		t.Fatalf("token = %q, want empty — no fallback to a standing credential", got)
	}
	if got := m.State(); got != ServiceTokenDegraded {
		t.Fatalf("state = %q, want degraded", got)
	}
}

// A stored blob that does not decrypt (wrong key, tampered bytes) refuses to
// arm. There is no plaintext fallback and no seed replay behind it.
func TestServiceTokenManager_UnreadableStoredCredentialRefusesToArm(t *testing.T) {
	stub := newZabAuthStub(t)
	st := &fakeTokenStore{}
	box := testBox(t)
	other := testBox(t)

	sealed, err := other.Seal([]byte(`{"rt":"someone-elses"}`))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := st.PutServiceRefreshToken(context.Background(), sealed); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	m := durableManager(stub, st, box, "seed-gen0")
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()
	if got := m.State(); got != ServiceTokenDegraded {
		t.Fatalf("state = %q, want degraded", got)
	}
	if got := stub.refreshCalls.Load(); got != 0 {
		t.Fatalf("refresh calls = %d, want 0 — an unreadable credential is not a licence to replay the seed", got)
	}
}

// No seed, nothing persisted: degraded, silent towards ZabAuth, and the boot
// still succeeds.
func TestServiceTokenManager_NoCredentialAtAll(t *testing.T) {
	stub := newZabAuthStub(t)
	st := &fakeTokenStore{}
	m := durableManager(stub, st, testBox(t), "")
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()
	if got := m.State(); got != ServiceTokenDegraded {
		t.Fatalf("state = %q, want degraded", got)
	}
	if got := stub.refreshCalls.Load(); got != 0 {
		t.Fatalf("refresh calls = %d, want 0", got)
	}
	if st.writeCount() != 0 {
		t.Fatalf("writes = %d, want 0", st.writeCount())
	}
}

// The rotation marker is written BEFORE the refresh call reaches ZabAuth. That
// ordering is the whole of the fail-closed guarantee: it is what a later boot
// reads to know the stored generation may already be spent.
func TestServiceTokenManager_MarkerIsWrittenBeforeTheRefreshCall(t *testing.T) {
	st := &fakeTokenStore{}
	box := testBox(t)

	var writesAtRefresh atomic.Int32
	stub := newZabAuthStub(t)
	// Re-point the manager at a server that inspects the store mid-call.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writesAtRefresh.Store(int32(st.writeCount()))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":       "access-g1",
			"refresh_token":      "refresh-g1",
			"expires_at":         time.Now().Add(time.Hour).Format(time.RFC3339Nano),
			"refresh_expires_at": time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339Nano),
		})
	}))
	defer srv.Close()

	m := durableManager(stub, st, box, "seed-gen0")
	m.RefreshURL = srv.URL + "/refresh"
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()
	if got := writesAtRefresh.Load(); got != 1 {
		t.Fatalf("writes seen while ZabAuth was answering = %d, want 1 (the marker)", got)
	}
}
