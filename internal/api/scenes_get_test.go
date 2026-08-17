package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

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

// TestGetRenderBundle_OnAirNeverResolvesThroughThisRoute pins the KNOWN,
// documented gap (clause 5, Amendment 3 territory, bluehost/host.go —
// Host.Take never records a sceneID): no {id}/?v= combination can ever
// resolve the on-air slot through this resolver, even with the exactly
// correct digest. This is not a regression this fix introduces — no
// legitimate render succeeded through this route before it either (the
// client-side version check independently refuses every response this
// branch could produce) — it is the honest consequence of keying by
// (scene_id, digest) against a slot whose scene_id is never set.
func TestGetRenderBundle_OnAirNeverResolvesThroughThisRoute(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Take("onair-1", "sha256:onair", program, nil, nil, nil); err != nil {
		t.Fatalf("Take on-air: %v", err)
	}
	host.SetBundle(bluehost.SlotOnAir, []byte(`"onair-bundle"`))

	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/scene-1/render-bundle?v=sha256:onair", nil)
	req.SetPathValue("id", "scene-1")
	rec := httptest.NewRecorder()
	getRenderBundle(deps)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (Take never records a sceneID for host.Serving to match), got %d: %s", rec.Code, rec.Body.String())
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
