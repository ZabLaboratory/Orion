package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

func lsmlRequest(sceneID, query string) *http.Request {
	path := "/api/v1/scenes/" + sceneID + "/lsml-bundle"
	if query != "" {
		path += "?" + query
	}
	r := httptest.NewRequest("GET", path, nil)
	r.SetPathValue("id", sceneID)
	return r
}

// getLSMLBundle is migrated off Store onto bluehost.Host.Bundle (#15,
// #331); there is no LSDP-mode gate anymore, so a missing/nil
// SceneIntent (no bluehost.Host wired at all) is the only structural
// "disabled" posture. {id} is NO LONGER vestigial (F1, Blue#345 / R13):
// it is required and matched, same posture as getRenderBundle — see
// resolveHostBundle's doc.
func TestLSMLBundle_NoHostWiredIs404(t *testing.T) {
	deps := PublicDeps{}
	w := httptest.NewRecorder()

	getLSMLBundle(deps)(w, lsmlRequest(uuid.NewString(), "v=sha256:abc"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// A ?v= that matches no loaded slot's digest 404s.
func TestLSMLBundle_UnknownVersionIs404(t *testing.T) {
	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: bluehost.NewHost()}}
	w := httptest.NewRecorder()

	getLSMLBundle(deps)(w, lsmlRequest(uuid.NewString(), "v=sha256:abc"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestLSMLBundle_MissingVersionIs404 replaces the removed "no ?v= ⇒
// whatever's current" fallback (F1): a request with no ?v= at all now
// 404s, even with a matching {id} and a loaded slot.
func TestLSMLBundle_MissingVersionIs404(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	sceneID := "scene-1"
	if err := host.Prepare(bluehost.SlotPreview, "instance-1", sceneID, "sha256:abc", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`{"root":{}}`))
	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	w := httptest.NewRecorder()

	getLSMLBundle(deps)(w, lsmlRequest(sceneID, ""))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with no ?v=", w.Code)
	}
}

// TestLSMLBundle_ServesMatchingSceneAndVersion is the positive case: a
// caller who supplies BOTH the correct {id} and ?v= is served.
func TestLSMLBundle_ServesMatchingSceneAndVersion(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	sceneID := "scene-1"
	if err := host.Prepare(bluehost.SlotPreview, "instance-1", sceneID, "sha256:abc", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`{"root":{}}`))
	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	w := httptest.NewRecorder()

	getLSMLBundle(deps)(w, lsmlRequest(sceneID, "v=sha256:abc"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"root":{}}` {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

// TestLSMLBundle_SceneIDMismatchIs404 mirrors the render-bundle case:
// the correct digest with the WRONG scene_id is refused.
func TestLSMLBundle_SceneIDMismatchIs404(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Prepare(bluehost.SlotPreview, "instance-1", "scene-1", "sha256:abc", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`{"root":{}}`))
	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	w := httptest.NewRecorder()

	getLSMLBundle(deps)(w, lsmlRequest("wrong-scene", "v=sha256:abc"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 on scene_id mismatch", w.Code)
	}
}
