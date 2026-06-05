package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/config"
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

// Acceptance #3 + "no-op without flag": in bespoke mode (the default)
// the LSML endpoint is inert — it returns 404 LSML_DISABLED and never
// touches the store, so a deploy with ORION_LSDP_MODE unset changes
// nothing. (deps.Store is nil here; the handler must short-circuit
// before dereferencing it.)
func TestLSMLBundle_DisabledInBespokeMode(t *testing.T) {
	deps := PublicDeps{Config: config.Config{LSDPMode: config.LSDPModeBespoke}}
	w := httptest.NewRecorder()

	getLSMLBundle(deps)(w, lsmlRequest(uuid.NewString(), "v=sha256:abc"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "LSML_DISABLED") {
		t.Fatalf("body = %s, want LSML_DISABLED", w.Body.String())
	}
}

// In dual mode, a request without ?v= is rejected: the LSML artifact is
// immutable + content-addressed, so the caller must pin the version.
func TestLSMLBundle_VersionRequiredInDualMode(t *testing.T) {
	deps := PublicDeps{Config: config.Config{LSDPMode: config.LSDPModeDual}}
	w := httptest.NewRecorder()

	getLSMLBundle(deps)(w, lsmlRequest(uuid.NewString(), ""))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "VERSION_REQUIRED") {
		t.Fatalf("body = %s, want VERSION_REQUIRED", w.Body.String())
	}
}

// A malformed scene id is a 400 before any store lookup.
func TestLSMLBundle_InvalidSceneID(t *testing.T) {
	deps := PublicDeps{Config: config.Config{LSDPMode: config.LSDPModeDual}}
	w := httptest.NewRecorder()

	getLSMLBundle(deps)(w, lsmlRequest("not-a-uuid", "v=sha256:abc"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
