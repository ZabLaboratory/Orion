package api

import (
	"context"
	"encoding/json"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadyReportsExactSharedNative(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), slog.Default())
	defer show.Stop()
	response := httptest.NewRecorder()
	deps := PublicDeps{Show: show, Config: config.Config{NativeLSDPAddress: "127.0.0.1:4520", NativeLSDPResource: "orion/state"}, NativeLSDPCheck: func(context.Context) error { return nil }}
	ready(deps)(response, httptest.NewRequest(http.MethodGet, "/ready", nil))
	var result struct {
		Native struct {
			Ready                   bool
			Wire, Address, Resource string
		} `json:"native_lsdp"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || !result.Native.Ready || result.Native.Address != "127.0.0.1:4520" || result.Native.Resource != "orion/state" || result.Native.Wire != "LSDP-TCP/2.0-draft2" {
		t.Fatalf("native proof missing: %s", response.Body.String())
	}
}
