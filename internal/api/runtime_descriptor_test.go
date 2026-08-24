package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/buildinfo"
)

func TestRuntimeDescriptorPublishesEmbeddedBlueRuntime(t *testing.T) {
	previous := buildinfo.BlueRuntimeSourceDigest
	buildinfo.BlueRuntimeSourceDigest = "sha256:" + strings.Repeat("a", 64)
	t.Cleanup(func() { buildinfo.BlueRuntimeSourceDigest = previous })
	recorder := httptest.NewRecorder()
	getRuntimeDescriptor(recorder, httptest.NewRequest("GET", "/api/v1/runtime/descriptor", nil))

	if recorder.Code != 200 {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var response runtimeDescriptorResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.SchemaVersion != "blue.runtime.descriptor.v1" {
		t.Fatalf("schema_version = %q", response.SchemaVersion)
	}
	if response.RuntimeAPI != "blue.runtime.api.v1" ||
		response.RuntimeABI != "blue-runtime-abi.v1" ||
		response.RuntimeModule != "blue-runtime-go" {
		t.Fatalf("unexpected runtime identity: %#v", response)
	}
	if response.SourceDigest == "" || response.SourceDigest == "unknown" {
		t.Fatalf("source digest is not package provenance: %q", response.SourceDigest)
	}
	if !strings.HasPrefix(response.SourceDigest, "sha256:") {
		t.Fatalf("source digest has invalid format: %q", response.SourceDigest)
	}
}
