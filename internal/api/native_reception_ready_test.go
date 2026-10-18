package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadyRejectsLostNativeReception(t *testing.T) {
	called := false
	response := httptest.NewRecorder()
	ready(PublicDeps{NativeLSDPCheck: func(context.Context) error { called = true; return errors.New("connection refused") }})(response, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if !called || response.Code != http.StatusServiceUnavailable {
		t.Fatalf("called=%v code=%d", called, response.Code)
	}
}
