// Package blueproject implements `orion.blue-solar-projection.v1`
// (ADR-BLUE-012 §6.7): the logical projection Orion derives from a
// blue-runtime-go StepResult and hands to the SAME LSML/LSDP wire
// internal/lsdp already speaks to Solar. It is not a new wire — §6.7 is
// explicit that "l'encapsulation LSML/LSDP et la projection broadcast
// restent Orion" — so this package produces exactly the leaf-path→value
// patch shape lsdp's sceneMirror.Forward already knows how to emit
// (lserver.Scene.Set / EmitWithCause), never a competing frame format.
package blueproject

import (
	"encoding/json"
)

// Target mirrors §6.7's `target=preview|program`.
type Target string

const (
	TargetPreview Target = "preview"
	TargetProgram Target = "program"
)

// Projection is `orion.blue-solar-projection.v1`. RenderRevision and
// ResourceRefs are left for the caller to stamp (they come from the
// zabcanvas.resolved-scene.v1 envelope, not from the runtime step) —
// this package's Project only derives OutputSequence and Patches from a
// StepResult, the part that genuinely comes from blue-runtime-go.
type Projection struct {
	SchemaVersion     string `json:"schema_version"`
	SceneDigest       string `json:"scene_digest"`
	RuntimeInstanceID string `json:"runtime_instance_id"`
	OutputSequence    uint64 `json:"output_sequence"`
	Target            Target `json:"target"`
	RenderRevision    string `json:"render_revision"`
	CorrelationID     string `json:"correlation_id"`
	// Patches is the leaf-path → value map, already filtered to the LSDP
	// §3.2.1 wire-legal shape (scalar or array-of-scalar). This is the
	// SAME shape internal/lsdp.sceneMirror.Forward hands to
	// lserver.Scene.Set/EmitWithCause; a caller wiring the WS surface
	// passes Patches straight through, no further translation needed.
	Patches map[string]json.RawMessage `json:"patches"`
}

// StepOutputs is the slice of blueruntime.StepResult this package
// consumes — kept as a narrow local shape (RuntimeSequence + Outputs)
// rather than importing the runtime package, so blueproject stays
// testable without constructing a live blue-runtime-go instance.
type StepOutputs struct {
	RuntimeSequence uint64
	Outputs         map[string]any
}

// Project derives a Projection from one step's outputs. Every value that
// is not wire-legal per §3.2.1 (a JSON object anywhere, top-level or
// nested) is DROPPED, never sent malformed — the same fail-safe posture
// internal/lsdp.isLSDPScalar already enforces on the legacy wire, so a
// Solar client never sees a divergent legality rule depending on which
// path produced the frame.
func Project(step StepOutputs, sceneDigest, instanceID, renderRevision, correlationID string, target Target) Projection {
	patches := make(map[string]json.RawMessage, len(step.Outputs))
	for path, val := range step.Outputs {
		raw, err := json.Marshal(val)
		if err != nil {
			continue
		}
		if !isWireLegal(raw) {
			continue
		}
		patches[path] = raw
	}
	return Projection{
		SchemaVersion:     "orion.blue-solar-projection.v1",
		SceneDigest:       sceneDigest,
		RuntimeInstanceID: instanceID,
		OutputSequence:    step.RuntimeSequence,
		Target:            target,
		RenderRevision:    renderRevision,
		CorrelationID:     correlationID,
		Patches:           patches,
	}
}

// isWireLegal mirrors internal/lsdp.isLSDPScalar's rule byte-for-byte:
// string / number / boolean / null, or an array of (recursively) legal
// values. A JSON object anywhere is forbidden. Duplicated rather than
// imported — internal/lsdp is not a dependency of this package, and the
// rule is a stable, tiny wire-format invariant, not internal plumbing
// worth coupling two packages over.
func isWireLegal(raw json.RawMessage) bool {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return false
	}
	return isScalarShape(decoded)
}

func isScalarShape(v any) bool {
	switch t := v.(type) {
	case nil, bool, float64, string:
		return true
	case []any:
		for _, e := range t {
			if !isScalarShape(e) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
