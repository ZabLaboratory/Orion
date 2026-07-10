package runtime

import (
	"encoding/json"
)

// The `overlay-app.set` exec op — the stream-level overlay-app control
// primitive (ADR 016 Prism §3.2, seed `core.overlay-app.set@1`, issue #283).
//
// Semantics. The op reads `app_id` (the operator-declared overlay app) and the
// OPTIONAL `running` / `on_air` booleans, then forwards the desired control
// state to the stream-level overlay mirror (the LSDP wire) via the Show seam
// wired at Load. The mirror accumulates the state at STREAM level (above any
// scene) and publishes the show-level `overlay_apps` frame (#292), which the
// Prism consumer (#360) reconciles into the app process + its composited
// window_capture item. The frame carries CONTROL only — {running, on_air}
// per app, never any app data (ADR 016 §3.2).
//
// running / on_air are OPTIONAL: a set may touch one dimension without
// clobbering the other (e.g. reveal on air while the process is already up).
// An absent dimension is left unchanged by the mirror.
//
// Construction-safe, NO error pin (like `show.emit`, Blue#73 shape): the mirror
// write is build-safe (no transport error), so the seed declares a single
// `then` exec output and no `error`. `then` fires synchronously on the same
// task. An unwired seam (bespoke mode / no mirror) no-ops the emission and
// STILL fires `then` — never a halt.
//
// R9 dormancy: like every exec op, it runs only on a scene with an installed
// ExecProgram, which no production path installs before the phase-4 gate (#87).

// OpOverlayAppSet is the runtime-canonical op name for `core.overlay-app.set@1`.
const OpOverlayAppSet = "overlay-app.set"

// execOverlayAppSet implements `overlay-app.set`. Data input `app_id` (string,
// required — an empty/absent one is logged and the emission skipped, but `then`
// still fires). Optional data inputs `running` / `on_air` (booleans; absent =
// that dimension unchanged). Out pin: `then` (immediate). No `error` pin.
func execOverlayAppSet(s *Scene, t *execTask, node *ExecNode, _ string) execOpOutcome {
	appID := pullString(s, t, node, "app_id")
	if appID == "" {
		// `app_id` is required by the seed; a missing one is an authoring gap
		// surfaced at validation, not a runtime crash. Skip the emission and
		// fall through to `then` (construction-safe).
		s.logger.Warn("overlay-app.set without app_id — emission skipped", "node", node.ID)
		return execOpOutcome{}
	}
	running := s.pullOptBool(t, node, "running")
	onAir := s.pullOptBool(t, node, "on_air")

	if s.overlayAppSet != nil {
		s.overlayAppSet(appID, running, onAir)
	} else {
		// Unwired seam (no mirror / bespoke mode): the emission is dropped, but
		// the chain is never amputated — `then` fires (construction-safe).
		s.logger.Debug("overlay-app.set on unwired scene — no mirror", "node", node.ID, "app_id", appID)
	}
	// Empty outcome → applyOutcome defaults to pushing the `then` target.
	return execOpOutcome{}
}

// pullOptBool reads an OPTIONAL boolean data input : nil when the port is
// unwired/absent (dimension unchanged), else a pointer to the decoded value.
// A present-but-non-boolean value is logged and treated as absent (nil) — the
// same fail-safe as pullBool, but preserving the optional contract.
func (s *Scene) pullOptBool(t *execTask, node *ExecNode, port string) *bool {
	raw, ok := s.pullData(t, node, port)
	if !ok {
		return nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		s.logger.Warn("exec: data input not a boolean", "node", node.ID, "port", port, "value", string(raw))
		return nil
	}
	return &b
}
