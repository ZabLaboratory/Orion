package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// Preview→air state hand-off seam tests (ADR Prism 005 Amendment 2
// §A2.2.d/e, Orion #256). They prove every Bastion VETO fail-closed at the
// handler boundary: the seed validation rejects before any Seed, never
// half-applies, and never airs a forged or live-targeted snapshot.

const snapSceneID = "22222222-2222-2222-2222-222222222222"
const snapVersion = "sha256:v1"

// --- unit layer: validateSnapshotForSeed / isReservedPath / role ---------

func keyspaceOf(paths ...string) map[string]struct{} {
	ks := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		ks[p] = struct{}{}
	}
	return ks
}

func TestValidateSnapshot_Accepts_WellFormedInKeyspace(t *testing.T) {
	snap := &stateSnapshot{Version: snapVersion, State: map[string]json.RawMessage{
		"score.blue": json.RawMessage(`3`),
		"team.name":  json.RawMessage(`"T1"`),
	}}
	clean, code, ok := validateSnapshotForSeed(snap, snapVersion, keyspaceOf("score.blue", "team.name"))
	if !ok {
		t.Fatalf("want accept, got code=%q", code)
	}
	if len(clean) != 2 {
		t.Fatalf("clean has %d paths, want 2", len(clean))
	}
}

func TestValidateSnapshot_VETO1_UnknownPathRejectedNoSeed(t *testing.T) {
	snap := &stateSnapshot{Version: snapVersion, State: map[string]json.RawMessage{
		"score.blue":      json.RawMessage(`3`),
		"forged.injected": json.RawMessage(`true`), // not in keyspace
	}}
	clean, code, ok := validateSnapshotForSeed(snap, snapVersion, keyspaceOf("score.blue"))
	if ok || code != snapshotPathUnknownCode {
		t.Fatalf("want reject SNAPSHOT_PATH_UNKNOWN, got ok=%v code=%q", ok, code)
	}
	if clean != nil {
		t.Fatal("VETO #4: no sanitised map on rejection (0 seed)")
	}
}

func TestValidateSnapshot_VETO2_ReservedNamespaceRejected(t *testing.T) {
	for _, p := range []string{
		"__system.harness", "__events.platform", "__inputs.platform.twitch.x",
		"__vars.bp.v", "__test.foo", "__resolved_source.s", "__anything",
	} {
		// Even WITH the reserved path inside the keyspace, it must be refused.
		snap := &stateSnapshot{Version: snapVersion, State: map[string]json.RawMessage{
			p: json.RawMessage(`1`),
		}}
		_, code, ok := validateSnapshotForSeed(snap, snapVersion, keyspaceOf(p))
		if ok || code != snapshotPathUnknownCode {
			t.Fatalf("reserved %q: want reject, got ok=%v code=%q", p, ok, code)
		}
	}
}

func TestIsReservedPath_SegmentBoundary(t *testing.T) {
	// `__eventsmaybe` is NOT caught by the __events prefix (segment-aware),
	// but IS caught by the `__` engine catch-all — both routes lead to
	// "reserved", which is the safe outcome.
	if !isReservedPath("__eventsmaybe") {
		t.Fatal("__ catch-all must reserve any double-underscore leaf")
	}
	if isReservedPath("score.blue") {
		t.Fatal("author path falsely reserved")
	}
	if !isReservedPath("__events") || !isReservedPath("__events.x") {
		t.Fatal("__events not reserved")
	}
}

func TestValidateSnapshot_VETO5_VersionMismatchRejected(t *testing.T) {
	snap := &stateSnapshot{Version: "sha256:OTHER", State: map[string]json.RawMessage{
		"score.blue": json.RawMessage(`3`),
	}}
	_, code, ok := validateSnapshotForSeed(snap, snapVersion, keyspaceOf("score.blue"))
	if ok || code != snapshotVersionMismatchCode {
		t.Fatalf("want SNAPSHOT_VERSION_MISMATCH, got ok=%v code=%q", ok, code)
	}
}

func TestValidateSnapshot_VersionEmptyRejected(t *testing.T) {
	snap := &stateSnapshot{Version: "", State: map[string]json.RawMessage{"score.blue": json.RawMessage(`3`)}}
	_, code, ok := validateSnapshotForSeed(snap, snapVersion, keyspaceOf("score.blue"))
	if ok || code != snapshotVersionMismatchCode {
		t.Fatalf("empty version: want mismatch, got ok=%v code=%q", ok, code)
	}
}

func TestValidateSnapshot_VETO6_MalformedValueRejected(t *testing.T) {
	snap := &stateSnapshot{Version: snapVersion, State: map[string]json.RawMessage{
		"score.blue": json.RawMessage(`{bad json`),
	}}
	_, code, ok := validateSnapshotForSeed(snap, snapVersion, keyspaceOf("score.blue"))
	if ok || code != snapshotMalformedCode {
		t.Fatalf("want SNAPSHOT_MALFORMED, got ok=%v code=%q", ok, code)
	}
}

func TestValidateSnapshot_VETO4_AtomicNoPartialSeed(t *testing.T) {
	// One good path, one out-of-keyspace path. The WHOLE snapshot must be
	// rejected with no sanitised map — never a partial seed.
	snap := &stateSnapshot{Version: snapVersion, State: map[string]json.RawMessage{
		"score.blue": json.RawMessage(`3`),
		"intruder":   json.RawMessage(`9`),
	}}
	clean, _, ok := validateSnapshotForSeed(snap, snapVersion, keyspaceOf("score.blue"))
	if ok || clean != nil {
		t.Fatal("VETO #4: partial-valid snapshot must yield 0 seed")
	}
}

func TestValidateSnapshot_VETO8_PathCountCap(t *testing.T) {
	st := make(map[string]json.RawMessage, snapshotMaxPaths+1)
	ks := make(map[string]struct{}, snapshotMaxPaths+1)
	for i := 0; i <= snapshotMaxPaths; i++ {
		p := "p" + strings.Repeat("x", 0) + itoa(i)
		st[p] = json.RawMessage(`1`)
		ks[p] = struct{}{}
	}
	snap := &stateSnapshot{Version: snapVersion, State: st}
	_, code, ok := validateSnapshotForSeed(snap, snapVersion, ks)
	if ok || code != snapshotTooLargeCode {
		t.Fatalf("want SNAPSHOT_TOO_LARGE, got ok=%v code=%q", ok, code)
	}
}

func TestSnapshotRoleRefused_ServiceAndViewer(t *testing.T) {
	for _, r := range []auth.Role{auth.RoleService, auth.RoleViewer, auth.RoleAnonymous} {
		if !snapshotRoleRefused(r) {
			t.Fatalf("role %q must be refused on the seed seam", r)
		}
	}
	for _, r := range []auth.Role{auth.RoleOperator, auth.RoleAdmin} {
		if snapshotRoleRefused(r) {
			t.Fatalf("role %q must be allowed", r)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// --- HTTP layer: both seams against a Show + a minimal fake store --------

// snapStore is a minimal Store fake: only the methods postActiveScene
// reaches when the target scene is already in the roster (GetScene,
// SetActiveSceneID). Every other method is unused and panics if reached —
// the tests preload the scene so loadSceneFromStore is never called.
type snapStore struct {
	store.Store
	scene      *store.Scene
	getErr     error
	activeSet  *uuid.UUID
	activeSets int
}

func (s *snapStore) GetScene(_ context.Context, _ uuid.UUID) (*store.Scene, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.scene, nil
}

func (s *snapStore) SetActiveSceneID(_ context.Context, id *uuid.UUID) error {
	s.activeSets++
	s.activeSet = id
	return nil
}

type snapFixture struct {
	mux   *http.ServeMux
	show  *runtime.Show
	store *snapStore
}

func newSnapFixture(t *testing.T, profile config.Profile) *snapFixture {
	t.Helper()
	m := obs.NewMetrics()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	show.SetExecMetrics(m)
	t.Cleanup(show.Stop)

	graph := &compiler.Graph{
		SceneID: snapSceneID, SceneVersion: snapVersion,
		Defaults: map[string]json.RawMessage{
			"score.blue": json.RawMessage(`0`),
			"team.name":  json.RawMessage(`""`),
		},
	}
	show.LoadExec(snapSceneID, graph, &compiler.RenderBundle{SceneVersion: snapVersion})

	ver := snapVersion
	st := &snapStore{scene: &store.Scene{
		ID:                  uuid.MustParse(snapSceneID),
		Name:                "snap",
		Status:              store.SceneActive,
		LatestPushedVersion: &ver,
	}}

	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{
		Logger:       testLogger(),
		Metrics:      m,
		Show:         show,
		Store:        st,
		AirValidator: &fakeAirValidator{verdict: true},
		Config:       config.Config{Profile: profile},
	})
	return &snapFixture{mux: mux, show: show, store: st}
}

func snapReq(t *testing.T, mux *http.ServeMux, method, path, role string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, &buf)
	if role != "" {
		r.Header.Set("X-Authenticated-User", "op")
		r.Header.Set("X-Authenticated-Role", role)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func sceneStateValue(t *testing.T, sc *runtime.Scene, leaf string) (string, bool) {
	t.Helper()
	_, _, state := sc.SnapshotState()
	v, ok := state[leaf]
	return string(v), ok
}

// --- seam (a) export -----------------------------------------------------

func TestExport_Seam_PreviewOnly_AntenneIs404(t *testing.T) {
	f := newSnapFixture(t, config.ProfileAntenne)
	w := snapReq(t, f.mux, "GET", "/api/v1/scenes/"+snapSceneID+"/state-snapshot", "operator", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("VETO #11: antenne export must 404, got %d", w.Code)
	}
}

func TestExport_Seam_Sidecar_ReturnsSnapshot(t *testing.T) {
	f := newSnapFixture(t, config.ProfileEmbeddedLocal)
	w := snapReq(t, f.mux, "GET", "/api/v1/scenes/"+snapSceneID+"/state-snapshot", "operator", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("sidecar export: got %d, want 200", w.Code)
	}
	var got stateSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != snapVersion {
		t.Fatalf("export version = %q, want %q", got.Version, snapVersion)
	}
	if _, ok := got.State["score.blue"]; !ok {
		t.Fatal("export missing seeded default leaf")
	}
}

func TestExport_Seam_RequiresOperator(t *testing.T) {
	f := newSnapFixture(t, config.ProfileEmbeddedLocal)
	w := snapReq(t, f.mux, "GET", "/api/v1/scenes/"+snapSceneID+"/state-snapshot", "viewer", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer export: got %d, want 403", w.Code)
	}
}

// --- seam (b) import + activation ----------------------------------------

func activeBody(snap *stateSnapshot) map[string]any {
	b := map[string]any{"scene_id": snapSceneID}
	if snap != nil {
		b["state_snapshot"] = snap
	}
	return b
}

func TestImport_Seam_SeedsThenActivates(t *testing.T) {
	f := newSnapFixture(t, config.ProfileAntenne)
	snap := &stateSnapshot{Version: snapVersion, State: map[string]json.RawMessage{
		"score.blue": json.RawMessage(`7`),
		"team.name":  json.RawMessage(`"GENG"`),
	}}
	w := snapReq(t, f.mux, "POST", "/api/v1/show/active-scene", "operator", activeBody(snap))
	if w.Code != http.StatusOK {
		t.Fatalf("seed+activate: got %d body=%s", w.Code, w.Body.String())
	}
	sc, err := f.show.Get(snapSceneID)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := sceneStateValue(t, sc, "score.blue"); !ok || v != "7" {
		t.Fatalf("RC-A2.5a: score.blue = %q ok=%v, want 7", v, ok)
	}
	if f.store.activeSets == 0 {
		t.Fatal("activation not persisted")
	}
}

func TestImport_Seam_NoSnapshot_BehavesAsBefore(t *testing.T) {
	f := newSnapFixture(t, config.ProfileAntenne)
	w := snapReq(t, f.mux, "POST", "/api/v1/show/active-scene", "operator", activeBody(nil))
	if w.Code != http.StatusOK {
		t.Fatalf("plain active-scene: got %d", w.Code)
	}
}

func TestImport_Seam_VETO3_SceneIsLive_NoMutation(t *testing.T) {
	f := newSnapFixture(t, config.ProfileAntenne)
	// Make the scene the active one first (no snapshot).
	if w := snapReq(t, f.mux, "POST", "/api/v1/show/active-scene", "operator", activeBody(nil)); w.Code != http.StatusOK {
		t.Fatalf("prime active: %d", w.Code)
	}
	sc, _ := f.show.Get(snapSceneID)
	before, _ := sceneStateValue(t, sc, "score.blue")

	snap := &stateSnapshot{Version: snapVersion, State: map[string]json.RawMessage{
		"score.blue": json.RawMessage(`42`),
	}}
	w := snapReq(t, f.mux, "POST", "/api/v1/show/active-scene", "operator", activeBody(snap))
	if w.Code != http.StatusConflict {
		t.Fatalf("VETO #3: targeting active must 409, got %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(sceneIsLiveCode)) {
		t.Fatalf("VETO #3: want SCENE_IS_LIVE, got %s", w.Body.String())
	}
	if after, _ := sceneStateValue(t, sc, "score.blue"); after != before {
		t.Fatalf("VETO #3: state mutated %q→%q on a refused seed", before, after)
	}
}

func TestImport_Seam_VETO1_UnknownPathRejectedNoSeed(t *testing.T) {
	f := newSnapFixture(t, config.ProfileAntenne)
	sc, _ := f.show.Get(snapSceneID)
	before, _ := sceneStateValue(t, sc, "score.blue")
	snap := &stateSnapshot{Version: snapVersion, State: map[string]json.RawMessage{
		"score.blue": json.RawMessage(`5`),
		"forged":     json.RawMessage(`true`),
	}}
	w := snapReq(t, f.mux, "POST", "/api/v1/show/active-scene", "operator", activeBody(snap))
	if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte(snapshotPathUnknownCode)) {
		t.Fatalf("VETO #1: want 400 SNAPSHOT_PATH_UNKNOWN, got %d %s", w.Code, w.Body.String())
	}
	if after, _ := sceneStateValue(t, sc, "score.blue"); after != before {
		t.Fatalf("VETO #1/#4: atomicity broken, %q→%q", before, after)
	}
}

func TestImport_Seam_VETO2_ReservedNamespaceRejected(t *testing.T) {
	f := newSnapFixture(t, config.ProfileAntenne)
	snap := &stateSnapshot{Version: snapVersion, State: map[string]json.RawMessage{
		"__events.platform.twitch.last_cheer": json.RawMessage(`{"forged":true}`),
	}}
	w := snapReq(t, f.mux, "POST", "/api/v1/show/active-scene", "operator", activeBody(snap))
	if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte(snapshotPathUnknownCode)) {
		t.Fatalf("VETO #2: want reject of reserved leaf, got %d %s", w.Code, w.Body.String())
	}
}

func TestImport_Seam_VETO5_VersionMismatch(t *testing.T) {
	f := newSnapFixture(t, config.ProfileAntenne)
	snap := &stateSnapshot{Version: "sha256:STALE", State: map[string]json.RawMessage{
		"score.blue": json.RawMessage(`1`),
	}}
	w := snapReq(t, f.mux, "POST", "/api/v1/show/active-scene", "operator", activeBody(snap))
	if w.Code != http.StatusConflict || !bytes.Contains(w.Body.Bytes(), []byte(snapshotVersionMismatchCode)) {
		t.Fatalf("VETO #5: want 409 SNAPSHOT_VERSION_MISMATCH, got %d %s", w.Code, w.Body.String())
	}
}

func TestImport_Seam_VETO9_ServiceRoleRefused(t *testing.T) {
	f := newSnapFixture(t, config.ProfileAntenne)
	snap := &stateSnapshot{Version: snapVersion, State: map[string]json.RawMessage{
		"score.blue": json.RawMessage(`1`),
	}}
	w := snapReq(t, f.mux, "POST", "/api/v1/show/active-scene", "service", activeBody(snap))
	if w.Code != http.StatusForbidden {
		t.Fatalf("VETO #9: service role must be 403, got %d", w.Code)
	}
}

func TestImport_Seam_VETO8_BodyTooLarge_413(t *testing.T) {
	f := newSnapFixture(t, config.ProfileAntenne)
	// Build a body well over snapshotMaxBytes (1 MiB) of valid JSON.
	big := strings.Repeat("a", snapshotMaxBytes+1024)
	raw := `{"scene_id":"` + snapSceneID + `","state_snapshot":{"version":"` + snapVersion +
		`","state":{"score.blue":"` + big + `"}}}`
	r := httptest.NewRequest("POST", "/api/v1/show/active-scene", strings.NewReader(raw))
	r.Header.Set("X-Authenticated-User", "op")
	r.Header.Set("X-Authenticated-Role", "operator")
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("VETO #8: oversized body must 413, got %d", w.Code)
	}
}

func TestImport_Seam_VETO6_MalformedValue(t *testing.T) {
	f := newSnapFixture(t, config.ProfileAntenne)
	// Inject a syntactically invalid leaf by hand (json.Encoder can't emit it).
	raw := `{"scene_id":"` + snapSceneID + `","state_snapshot":{"version":"` + snapVersion +
		`","state":{"score.blue":not-json}}}`
	r := httptest.NewRequest("POST", "/api/v1/show/active-scene", strings.NewReader(raw))
	r.Header.Set("X-Authenticated-User", "op")
	r.Header.Set("X-Authenticated-Role", "operator")
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	// A wholly invalid body is a decode error (400 invalid body); the
	// per-leaf SNAPSHOT_MALFORMED is covered by the unit test. Either way
	// it must NOT 200 and must NOT seed.
	if w.Code == http.StatusOK {
		t.Fatalf("malformed body must not succeed, got 200")
	}
}

// guard: the seam never airs without the air-eligibility gate.
func TestImport_Seam_NotValidated_RefusesWithSnapshot(t *testing.T) {
	f := newSnapFixture(t, config.ProfileAntenne)
	// Swap in a validator that says "not eligible".
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{
		Logger: testLogger(), Metrics: obs.NewMetrics(), Show: f.show, Store: f.store,
		AirValidator: &fakeAirValidator{verdict: false},
		Config:       config.Config{Profile: config.ProfileAntenne},
	})
	snap := &stateSnapshot{Version: snapVersion, State: map[string]json.RawMessage{
		"score.blue": json.RawMessage(`1`),
	}}
	w := snapReq(t, mux, "POST", "/api/v1/show/active-scene", "operator", activeBody(snap))
	if w.Code != http.StatusConflict || !bytes.Contains(w.Body.Bytes(), []byte(sceneNotValidatedCode)) {
		t.Fatalf("VETO #7: non-air-eligible must refuse, got %d %s", w.Code, w.Body.String())
	}
}
