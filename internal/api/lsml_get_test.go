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
// #331) — {id} is now vestigial (kept for URL-shape compatibility with
// Solar/Prism), so a missing/nil SceneIntent (no bluehost.Host wired at
// all) is the only "disabled" posture left; there is no LSDP-mode gate
// anymore.
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

// Without ?v=, whatever slot is loaded answers (legacy's "no ?v= ⇒
// latest" default, ported to "no ?v= ⇒ whatever's current").
func TestLSMLBundle_ServesLoadedSlotWithoutVersionPin(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Prepare(bluehost.SlotPreview, "instance-1", "scene-1", "sha256:abc", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`{"root":{}}`))
	deps := PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}}
	w := httptest.NewRecorder()

	getLSMLBundle(deps)(w, lsmlRequest(uuid.NewString(), ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"root":{}}` {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}
