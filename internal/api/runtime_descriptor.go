package api

import (
	"encoding/json"
	"net/http"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/buildinfo"
)

type runtimeDescriptorResponse struct {
	SchemaVersion        string   `json:"schema_version"`
	RuntimeAPI           string   `json:"runtime_api"`
	RuntimeModule        string   `json:"runtime_module"`
	RuntimeModuleVersion string   `json:"runtime_module_version"`
	RuntimeABI           string   `json:"runtime_abi"`
	Canonicalization     string   `json:"canonicalization"`
	ProgramSchemas       []string `json:"program_schemas"`
	SupportedOpcodes     []string `json:"supported_opcodes"`
	SourceDigest         string   `json:"source_digest"`
	SourceRevision       string   `json:"source_revision"`
}

func getRuntimeDescriptor(w http.ResponseWriter, _ *http.Request) {
	descriptor := blueruntime.NewRuntime().Descriptor()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(runtimeDescriptorResponse{
		SchemaVersion:        "blue.runtime.descriptor.v1",
		RuntimeAPI:           descriptor.APIVersion,
		RuntimeModule:        descriptor.ModuleName,
		RuntimeModuleVersion: descriptor.ModuleVersion,
		RuntimeABI:           descriptor.RuntimeABI,
		Canonicalization:     descriptor.Canonicalization,
		ProgramSchemas:       descriptor.ProgramSchemas,
		SupportedOpcodes:     descriptor.SupportedOpcodes,
		SourceDigest:         buildinfo.BlueRuntimeSourceDigest,
		SourceRevision:       buildinfo.BlueRuntimeSourceRevision,
	})
}
