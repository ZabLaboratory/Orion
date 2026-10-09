package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

func TestNativeSessionLeaseAndCloseAreOperatorGated(t *testing.T) {
	manager := runtime.NewTestSessionManager(runtime.NewComputeRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute)
	defer manager.Close()
	id, _ := manager.Open(context.Background(), "scene", &compiler.Graph{SceneVersion: "v1"}, &compiler.RenderBundle{})
	flushed := false
	deps := PublicDeps{Test: manager, NativeLSDPFlush: func(context.Context) error { flushed = true; return nil }}
	request := func(role string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/show/test-sessions/"+id+"/lease", nil)
		r.SetPathValue("session", id)
		r.Header.Set("X-Authenticated-User", "operator")
		r.Header.Set("X-Authenticated-Role", role)
		return r
	}
	rec := httptest.NewRecorder()
	renewNativeTestSession(deps)(rec, request("viewer"))
	if rec.Code != http.StatusForbidden {
		t.Fatal("viewer renewed operator test lease", rec.Code)
	}
	rec = httptest.NewRecorder()
	renewNativeTestSession(deps)(rec, request("operator"))
	if rec.Code != http.StatusOK {
		t.Fatal("lease renewal failed", rec.Code)
	}
	rec = httptest.NewRecorder()
	closeNativeTestSession(deps)(rec, request("viewer"))
	if rec.Code != http.StatusForbidden || flushed {
		t.Fatal("viewer closed operator test session")
	}
	rec = httptest.NewRecorder()
	closeNativeTestSession(deps)(rec, request("operator"))
	if rec.Code != http.StatusNoContent || !flushed {
		t.Fatal("close was not acknowledged")
	}
	rec = httptest.NewRecorder()
	renewNativeTestSession(deps)(rec, request("operator"))
	if rec.Code != http.StatusGone {
		t.Fatal("closed session revived")
	}
}
