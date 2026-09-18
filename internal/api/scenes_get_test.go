package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

func TestGetRenderBundle_EditablePreviewExactGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slot := runtime.NewPreviewSlot(ctx, runtime.NewComputeRegistry(), noopPreviewWire{}, slog.Default())
	defer slot.Close()
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:editable", Root: compiler.LayoutNode{Kind: "frame"}}
	if err := slot.ActivateStatic("editable-scene", bundle); err != nil {
		t.Fatal(err)
	}
	deps := PublicDeps{EditablePreview: slot}
	for _, tc := range []struct {
		id, version string
		status      int
	}{
		{"editable-scene", "sha256:editable", http.StatusOK},
		{"editable-scene", "sha256:old", http.StatusNotFound},
		{"other-scene", "sha256:editable", http.StatusNotFound},
		{"editable-scene", "", http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/"+tc.id+"/render-bundle?v="+tc.version, nil)
		req.SetPathValue("id", tc.id)
		rec := httptest.NewRecorder()
		getRenderBundle(deps)(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("%s/%s: got %d, want %d: %s", tc.id, tc.version, rec.Code, tc.status, rec.Body.String())
		}
	}
}

func TestGetRenderBundle_NoHostWiredIs404(t *testing.T) {
	deps := PublicDeps{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/any/render-bundle", nil)
	req.SetPathValue("id", "any")
	rec := httptest.NewRecorder()
	getRenderBundle(deps)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// TestGetRenderBundle_MissingVersionIs404 is the F1 fix's core behaviour
// (Blue#345 / R13 VETO 1): the pre-fix "no ?v= ⇒ first non-empty slot,
// on-air first" fallback is gone. A syntactically valid {id} with no ?v=
// at all — exactly what the unauthenticated harvest chain sent — now 404s
// instead of returning the live antenna bundle.
func TestGetRenderBundle_MissingVersionIs404(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Prepare(bluehost.SlotPreview, "preview-1", "scene-1", "sha256:preview", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare preview: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`"preview-bundle"`))

	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/scene-1/render-bundle", nil)
	req.SetPathValue("id", "scene-1")
	rec := httptest.NewRecorder()
	getRenderBundle(deps)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 with no ?v=, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGetRenderBundle_SceneIDMismatchIs404 is F1's second half: a caller
// who knows the exact digest but not the scene it belongs to is refused
// — {id} is no longer vestigial, it must match too.
func TestGetRenderBundle_SceneIDMismatchIs404(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Prepare(bluehost.SlotPreview, "preview-1", "scene-1", "sha256:preview", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare preview: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`"preview-bundle"`))

	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/wrong-scene/render-bundle?v=sha256:preview", nil)
	req.SetPathValue("id", "wrong-scene")
	rec := httptest.NewRecorder()
	getRenderBundle(deps)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on scene_id mismatch (correct digest, wrong id), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGetRenderBundle_TakenSceneResolvesThroughThisRoute is the FULL
// on-air path proof (ORION-TAKE-SLOT-IDENTITY, Blue#345): a scene taken
// to the antenna is subsequently resolvable through the exact public
// resolver a real client hits — not merely that host.Serving reports
// true in isolation. Before this fix, Host.Take never recorded a
// sceneID (only Prepare did), so no {id}/?v= combination could ever
// resolve the on-air slot here — every stateless on-air occupation was
// unservable through this route, unconditionally, program or not. This
// test used to pin that gap as an accepted, documented limitation
// (TestGetRenderBundle_OnAirNeverResolvesThroughThisRoute); it now pins
// the closure instead.
func TestGetRenderBundle_TakenSceneResolvesThroughThisRoute(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Take("onair-1", "scene-1", "sha256:onair", program, nil, nil, nil); err != nil {
		t.Fatalf("Take on-air: %v", err)
	}
	host.SetBundle(bluehost.SlotOnAir, []byte(`"onair-bundle"`))

	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/scene-1/render-bundle?v=sha256:onair", nil)
	req.SetPathValue("id", "scene-1")
	rec := httptest.NewRecorder()
	getRenderBundle(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 — a scene Taken to the antenna must resolve through resolveHostBundle, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `"onair-bundle"` {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

// TestGetRenderBundle_TakenSceneWrongIDStillRejected is the negative
// symmetric check: fixing Take's sceneID must not turn resolveHostBundle
// into a "any id resolves the antenna" oracle again — the WRONG id
// still 404s even against a correctly-recorded on-air occupation.
func TestGetRenderBundle_TakenSceneWrongIDStillRejected(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Take("onair-1", "scene-1", "sha256:onair", program, nil, nil, nil); err != nil {
		t.Fatalf("Take on-air: %v", err)
	}
	host.SetBundle(bluehost.SlotOnAir, []byte(`"onair-bundle"`))

	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/wrong-scene/render-bundle?v=sha256:onair", nil)
	req.SetPathValue("id", "wrong-scene")
	rec := httptest.NewRecorder()
	getRenderBundle(deps)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on wrong scene_id even with the correct digest, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGetRenderBundle_PreviewSymmetryUnaffected is the symmetry check
// (F1's own point dur): the preview path, which already worked (Prepare
// always set sceneID), must keep working exactly as before, and remain
// scoped to preview only — it must NOT resolve as if it were on-air.
func TestGetRenderBundle_PreviewSymmetryUnaffected(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Prepare(bluehost.SlotPreview, "preview-1", "scene-1", "sha256:preview", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare preview: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`"preview-bundle"`))

	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/scene-1/render-bundle?v=sha256:preview", nil)
	req.SetPathValue("id", "scene-1")
	rec := httptest.NewRecorder()
	getRenderBundle(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected preview to keep resolving, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `"preview-bundle"` {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}

	// The SAME (id, v) must not accidentally resolve on-air too — the
	// on-air slot is empty here, so a request pinned to the on-air digest
	// must still 404.
	onAirReq := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/scene-1/render-bundle?v=sha256:onair-never-taken", nil)
	onAirReq.SetPathValue("id", "scene-1")
	onAirRec := httptest.NewRecorder()
	getRenderBundle(deps)(onAirRec, onAirReq)
	if onAirRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a digest no slot is serving, got %d", onAirRec.Code)
	}
}

func TestGetRenderBundle_VersionPinSelectsMatchingSlot(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Prepare(bluehost.SlotPreview, "preview-1", "scene-1", "sha256:preview", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare preview: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`"preview-bundle"`))

	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/scene-1/render-bundle?v=sha256:preview", nil)
	req.SetPathValue("id", "scene-1")
	rec := httptest.NewRecorder()
	getRenderBundle(deps)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `"preview-bundle"` {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestGetRenderBundle_UnknownVersionIs404(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Prepare(bluehost.SlotPreview, "preview-1", "scene-1", "sha256:preview", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`"preview-bundle"`))

	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/scene-1/render-bundle?v=sha256:doesnotexist", nil)
	req.SetPathValue("id", "scene-1")
	rec := httptest.NewRecorder()
	getRenderBundle(deps)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestGetOperatorInputs_ExtractsKeyWhenPresent(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Prepare(bluehost.SlotPreview, "preview-1", "scene-1", "sha256:preview", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`{"operator_inputs":[{"path":"score.home"}]}`))

	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/scene-1/operator-inputs?v=sha256:preview", nil)
	req.SetPathValue("id", "scene-1")
	rec := httptest.NewRecorder()
	getOperatorInputs(deps)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		SceneVersion   string          `json:"scene_version"`
		OperatorInputs json.RawMessage `json:"operator_inputs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SceneVersion != "sha256:preview" {
		t.Fatalf("unexpected scene_version: %q", resp.SceneVersion)
	}
	if string(resp.OperatorInputs) != `[{"path":"score.home"}]` {
		t.Fatalf("unexpected operator_inputs: %s", resp.OperatorInputs)
	}
}

func TestGetOperatorInputs_AbsentKeyYieldsEmptyArray(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Prepare(bluehost.SlotPreview, "preview-1", "scene-1", "sha256:preview", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`{"root":{}}`))

	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/scene-1/operator-inputs?v=sha256:preview", nil)
	req.SetPathValue("id", "scene-1")
	rec := httptest.NewRecorder()
	getOperatorInputs(deps)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OperatorInputs json.RawMessage `json:"operator_inputs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.OperatorInputs) != `[]` {
		t.Fatalf("expected empty array, got %s", resp.OperatorInputs)
	}
}

// TestGetOperatorInputs_SceneIDMismatchIs404 proves the resolver's F1
// hardening protects getOperatorInputs too — it calls resolveHostBundle
// directly (not through serveHostBundle), so a fix scoped to only one
// call site would have left this route open.
func TestGetOperatorInputs_SceneIDMismatchIs404(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Prepare(bluehost.SlotPreview, "preview-1", "scene-1", "sha256:preview", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`{"operator_inputs":[{"path":"score.home"}]}`))

	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/wrong-scene/operator-inputs?v=sha256:preview", nil)
	req.SetPathValue("id", "wrong-scene")
	rec := httptest.NewRecorder()
	getOperatorInputs(deps)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on scene_id mismatch, got %d: %s", rec.Code, rec.Body.String())
	}
}
