package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// httptest coverage of the external completion endpoint (B-syswrite,
// issue #86): the 4 ordered gates, the uniform 202 (forged/stale/
// cross-scene reports are indistinguishable from accepted ones), the
// rejected-completion metrics, and R9 inertness (a scene without exec
// never resumes anything).

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// animFixture loads one exec-bearing scene into a Show, fires the
// animation.play chain and returns the wake key the command emitted
// (read off a subscriber snapshot — the renderer's view).
type animFixture struct {
	deps    PublicDeps
	show    *runtime.Show
	metrics *obs.Metrics
	sceneID string
	wakeKey string
}

func newAnimFixture(t *testing.T, sceneID string) *animFixture {
	t.Helper()
	m := obs.NewMetrics()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	show.SetExecMetrics(m)
	t.Cleanup(show.Stop)

	graph := &compiler.Graph{
		SceneID: sceneID, SceneVersion: "sha256:api-anim",
		Defaults: map[string]json.RawMessage{"__vars.bp.done": json.RawMessage(`null`)},
	}
	prog := &runtime.ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*runtime.ExecNode{
			"anim": {ID: "anim", Op: runtime.OpAnimationPlay,
				Config: map[string]json.RawMessage{
					"overlay_id":   json.RawMessage(`"ov"`),
					"animation_id": json.RawMessage(`"fade"`),
					// Far deadline: the real clock's fallback never
					// interferes with these tests.
					"duration_seconds": json.RawMessage(`3600`),
				},
				Next: map[string]runtime.ExecTarget{"completed": {Node: "set.done"}}},
			"set.done": {ID: "set.done", Op: runtime.OpVariableSet,
				Config: map[string]json.RawMessage{
					"variable": json.RawMessage(`"done"`), "value": json.RawMessage(`"done"`),
				}},
		},
		Entrypoints: map[string]runtime.ExecEntry{"e": {Target: runtime.ExecTarget{Node: "anim"}}},
	}
	show.LoadExec(sceneID, graph, &compiler.RenderBundle{SceneVersion: "sha256:api-anim"}, prog)
	scene, err := show.Get(sceneID)
	if err != nil {
		t.Fatal(err)
	}
	if !scene.FireExec("e", "test") {
		t.Fatal("inbox full")
	}

	f := &animFixture{
		deps: PublicDeps{Logger: testLogger(), Metrics: m, Show: show},
		show: show, metrics: m, sceneID: sceneID,
	}
	f.wakeKey = f.waitLeaf(t, sceneID, "__anim.ov.1", 2*time.Second)
	return f
}

// waitLeaf polls subscriber snapshots until the leaf appears, then
// returns the wake key it carries (or the raw value for assertions).
func (f *animFixture) waitLeaf(t *testing.T, sceneID, leaf string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if raw, ok := f.snapshotLeaf(t, sceneID, leaf); ok {
			var cmd struct {
				WakeKey string `json:"wake_key"`
			}
			if err := json.Unmarshal(raw, &cmd); err == nil && cmd.WakeKey != "" {
				return cmd.WakeKey
			}
			return string(raw)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("leaf %s never appeared", leaf)
	return ""
}

func (f *animFixture) snapshotLeaf(t *testing.T, sceneID, leaf string) (json.RawMessage, bool) {
	t.Helper()
	scene, err := f.show.Get(sceneID)
	if err != nil {
		t.Fatal(err)
	}
	sub, snap := scene.Subscribe(16)
	defer sub.Close()
	v, ok := snap.State[leaf]
	return v, ok
}

func (f *animFixture) doneLeaf(t *testing.T) string {
	t.Helper()
	v, ok := f.snapshotLeaf(t, f.sceneID, "__vars.bp.done")
	if !ok {
		return ""
	}
	return string(v)
}

// completionReq builds the POST with simulated ZabGate trust headers.
func completionReq(sceneID, body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest("POST", "/api/v1/scenes/"+sceneID+"/exec/completion",
		strings.NewReader(body))
	r.SetPathValue("id", sceneID)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// rendererHeaders is the legitimate renderer identity: role=service +
// the `__system.anim.report` scope in X-Authenticated-Paths.
func rendererHeaders() map[string]string {
	return map[string]string{
		"X-Authenticated-User":  "pulsar-cef",
		"X-Authenticated-Role":  "service",
		"X-Authenticated-Paths": "__system.anim.report",
	}
}

func reportBody(wakeKey string) string {
	return `{"wake_key":"` + wakeKey + `","kind":"animation","result":{"ok":true},"error":null}`
}

func postCompletion(t *testing.T, f *animFixture, sceneID, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	postExecCompletion(f.deps)(w, completionReq(sceneID, body, headers))
	return w
}

func rejected(f *animFixture, sceneID, reason string) float64 {
	return testutil.ToFloat64(f.metrics.ComplRejected.WithLabelValues(sceneID, reason))
}

func waitCounter(t *testing.T, desc string, get func() float64, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if get() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s = %v, want %v", desc, get(), want)
}

// notResumed asserts (after a beat) that the completed chain never ran.
func notResumed(t *testing.T, f *animFixture) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if v := f.doneLeaf(t); v != `null` {
		t.Fatalf("continuation resumed: done = %s", v)
	}
}

// TestCompletion_HappyPathResumes: legitimate report → 202, the parked
// continuation of THAT scene resumes (gates 1-4 all pass).
func TestCompletion_HappyPathResumes(t *testing.T) {
	f := newAnimFixture(t, "scene-happy")
	w := postCompletion(t, f, f.sceneID, reportBody(f.wakeKey), rendererHeaders())
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.doneLeaf(t) != `"done"` {
		if !time.Now().Before(deadline) {
			t.Fatalf("continuation never resumed: done = %s", f.doneLeaf(t))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestCompletion_Gate1_RoleScope: missing scope, wrong scope and
// non-service roles are dropped+counted; the response is BYTE-IDENTICAL
// to an accepted one (no continuation-existence leak).
func TestCompletion_Gate1_RoleScope(t *testing.T) {
	f := newAnimFixture(t, "scene-role")
	cases := []map[string]string{
		{}, // anonymous
		{"X-Authenticated-User": "u", "X-Authenticated-Role": "service"},                                                 // no paths
		{"X-Authenticated-User": "u", "X-Authenticated-Role": "service", "X-Authenticated-Paths": "__inputs.platform.*"}, // wrong scope
		{"X-Authenticated-User": "u", "X-Authenticated-Role": "operator"},                                                // role != service
		{"X-Authenticated-User": "u", "X-Authenticated-Role": "viewer", "X-Authenticated-Paths": "__system.anim.report"},
	}
	var bodies []string
	for _, h := range cases {
		w := postCompletion(t, f, f.sceneID, reportBody(f.wakeKey), h)
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (uniform)", w.Code)
		}
		bodies = append(bodies, w.Body.String())
	}
	if got := rejected(f, f.sceneID, "role"); got != float64(len(cases)) {
		t.Fatalf("role rejections = %v, want %d", got, len(cases))
	}
	notResumed(t, f)
	// Same body as a legitimately-accepted report.
	ok := postCompletion(t, f, f.sceneID, reportBody(f.wakeKey), rendererHeaders())
	for _, b := range bodies {
		if b != ok.Body.String() {
			t.Fatalf("rejected body %q differs from accepted %q — leaks", b, ok.Body.String())
		}
	}
}

// TestCompletion_Gate2_SceneMatch: a wake key reported against ANOTHER
// scene never resolves (per-scene parked map — no cross-scene resume),
// and an unknown scene id drops at the endpoint.
func TestCompletion_Gate2_SceneMatch(t *testing.T) {
	f := newAnimFixture(t, "scene-a")
	// Plain prod-like scene B, no exec at all.
	f.show.Load("scene-b", &compiler.Graph{SceneID: "scene-b", SceneVersion: "sha256:api-anim"},
		&compiler.RenderBundle{SceneVersion: "sha256:api-anim"})

	// A's key posted to B: 202, dropped inside B as unknown.
	w := postCompletion(t, f, "scene-b", reportBody(f.wakeKey), rendererHeaders())
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	waitCounter(t, "unknown drops on scene-b", func() float64 { return rejected(f, "scene-b", "unknown") }, 1)
	notResumed(t, f) // A's continuation untouched

	// Unknown scene id: endpoint-level drop, reason "scene".
	w = postCompletion(t, f, "no-such-scene", reportBody(f.wakeKey), rendererHeaders())
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if got := rejected(f, "no-such-scene", "scene"); got != 1 {
		t.Fatalf("scene rejections = %v, want 1", got)
	}
}

// TestCompletion_Gate3_StaleEpoch: cancellation bumps the epoch; the
// old stamped key drops on `orion_exec_resume_stale_total` (the
// existing inner gate), resuming nothing — still 202.
func TestCompletion_Gate3_StaleEpoch(t *testing.T) {
	f := newAnimFixture(t, "scene-stale")
	scene, err := f.show.Get(f.sceneID)
	if err != nil {
		t.Fatal(err)
	}
	scene.CancelExec()
	time.Sleep(20 * time.Millisecond) // let the cancel land

	w := postCompletion(t, f, f.sceneID, reportBody(f.wakeKey), rendererHeaders())
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	waitCounter(t, "stale drops", func() float64 {
		return testutil.ToFloat64(f.metrics.ResumeStale.WithLabelValues(f.sceneID))
	}, 1)
	notResumed(t, f)
}

// TestCompletion_Gate4_ForgedKey: a same-version forged key matching no
// parked continuation drops as "unknown", resuming nothing — still 202.
func TestCompletion_Gate4_ForgedKey(t *testing.T) {
	f := newAnimFixture(t, "scene-forged")
	w := postCompletion(t, f, f.sceneID, reportBody("wk|sha256:api-anim|0|999"), rendererHeaders())
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	waitCounter(t, "unknown drops", func() float64 { return rejected(f, f.sceneID, "unknown") }, 1)
	notResumed(t, f)
}

// TestCompletion_MalformedAndWrongKind: garbage bodies, a missing wake
// key and a non-animation kind all drop+count — still 202.
func TestCompletion_MalformedAndWrongKind(t *testing.T) {
	f := newAnimFixture(t, "scene-malformed")
	for _, body := range []string{`not json`, `{}`, `{"kind":"animation"}`} {
		if w := postCompletion(t, f, f.sceneID, body, rendererHeaders()); w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", w.Code)
		}
	}
	if got := rejected(f, f.sceneID, "malformed"); got != 3 {
		t.Fatalf("malformed rejections = %v, want 3", got)
	}
	w := postCompletion(t, f, f.sceneID, `{"wake_key":"`+f.wakeKey+`","kind":"http"}`, rendererHeaders())
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if got := rejected(f, f.sceneID, "kind"); got != 1 {
		t.Fatalf("kind rejections = %v, want 1", got)
	}
	notResumed(t, f)
}

// TestCompletion_R9_InertWithoutExec: against a prod-like scene (no
// exec program — the R9 dormancy state until #87), any report is an
// inert 202 drop; nothing in the scene changes.
func TestCompletion_R9_InertWithoutExec(t *testing.T) {
	m := obs.NewMetrics()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	show.SetExecMetrics(m)
	t.Cleanup(show.Stop)
	show.Load("prod-scene", &compiler.Graph{SceneID: "prod-scene", SceneVersion: "sha256:prod"},
		&compiler.RenderBundle{SceneVersion: "sha256:prod"})
	f := &animFixture{deps: PublicDeps{Logger: testLogger(), Metrics: m, Show: show}, show: show, metrics: m, sceneID: "prod-scene"}

	w := postCompletion(t, f, "prod-scene", reportBody("wk|sha256:prod|0|1"), rendererHeaders())
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	waitCounter(t, "inert unknown drop", func() float64 { return rejected(f, "prod-scene", "unknown") }, 1)
}
