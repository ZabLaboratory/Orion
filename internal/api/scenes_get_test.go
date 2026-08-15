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

func TestGetRenderBundle_ServesOnAirOverPreview(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Prepare(bluehost.SlotPreview, "preview-1", "scene-1", "sha256:preview", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare preview: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`"preview-bundle"`))
	if err := host.Take("onair-1", "sha256:onair", program, nil, nil, nil); err != nil {
		t.Fatalf("Take on-air: %v", err)
	}
	host.SetBundle(bluehost.SlotOnAir, []byte(`"onair-bundle"`))

	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/any/render-bundle", nil)
	req.SetPathValue("id", "any")
	rec := httptest.NewRecorder()
	getRenderBundle(deps)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `"onair-bundle"` {
		t.Fatalf("expected on-air bundle to win with no ?v=, got %s", rec.Body.String())
	}
	if rec.Header().Get("ETag") != `"sha256:onair"` {
		t.Fatalf("unexpected ETag: %q", rec.Header().Get("ETag"))
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
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/any/render-bundle?v=sha256:preview", nil)
	req.SetPathValue("id", "any")
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
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/any/render-bundle?v=sha256:doesnotexist", nil)
	req.SetPathValue("id", "any")
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
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/any/operator-inputs", nil)
	req.SetPathValue("id", "any")
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
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/any/operator-inputs", nil)
	req.SetPathValue("id", "any")
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
