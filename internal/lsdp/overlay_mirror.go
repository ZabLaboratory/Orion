package lsdp

// Stream-level overlay-app control mirror (ADR 016 Prism §3.2, issue #283).
//
// A `core.overlay-app.set@1` node, used in a stream-level rule (ADR 009),
// sets the desired `{running, on_air}` state of an operator-declared overlay
// app at STREAM level — above any single scene, so it survives scene switches
// (the overlay-app control is not a property of one scene of the show). The
// state is carried to the Prism consumer (lot 5, #360) as LSDP leaves so Prism
// reconciles the app process + its composited window_capture item.
//
// # Wire leaf format (consumed by Prism #360)
//
//	__overlay.<app_id>.running : true|false   (boolean scalar leaf)
//	__overlay.<app_id>.on_air  : true|false   (boolean scalar leaf)
//
// The ADR notes the logical leaf as `__overlay.<app_id> : {running, on_air}`;
// on the wire it is realized as TWO scalar boolean leaves under the reserved
// `__overlay.<app_id>.` prefix, because LSDP §3.2.1 forbids an object-valued
// leaf (isScalarShape, mirror.go). Two booleans, never any app DATA — the leaf
// carries control state only.
//
// The `__overlay.` reserved prefix is NOT a scene leaf: it rides the wire
// directly via the kit scene (Emit), bypassing the per-scene bound-leaf gate
// (sceneMirror.Forward) which governs only the reactive scene tap. Booleans
// pass the §3.2.1 scalar shape contract.
//
// # Partial updates
//
// The node's `running` / `on_air` are both OPTIONAL: a set may carry only one
// dimension (e.g. reveal on air without touching the process). Each dimension
// is accumulated independently; a set emits only the dimension(s) it carries,
// and the replay re-emits every dimension seen so far.
//
// # Derived cache / persistence
//
// The Wire holds the authoritative-for-LSDP map (the derived cache; memory
// only — no DB, no goose: restart loses it, the durable state belongs to the
// app itself, ADR 016 RC #11). On a scene switch the kit migrates subscribers
// to the destination scene with that scene's snapshot — which does NOT carry
// the stream-level overlay state — so SetActive replays the whole map onto the
// new active scene. A late joiner therefore sees the overlay control state in
// the destination scene's accumulated state.

const overlayLeafPrefix = "__overlay."

// overlayState is the accumulated per-app control state (the derived cache).
// The *Set flags distinguish "never set" from "set to false", so a replay
// re-emits only the dimensions an operator actually touched.
type overlayState struct {
	running    bool
	runningSet bool
	onAir      bool
	onAirSet   bool
}

// overlayLeaf builds the reserved wire leaf for one dimension of an app.
func overlayLeaf(appID, dim string) string {
	return overlayLeafPrefix + appID + "." + dim
}

// EmitOverlayApp records the stream-level overlay control state and emits the
// delta on the active kit scene. running / on_air are optional (nil = leave
// that dimension unchanged) so a set can touch one dimension without clobbering
// the other. With no active scene yet, it only stores — the next SetActive
// replays it. Safe for concurrent use (the op runs on a scene goroutine;
// SetActive on the show's).
func (w *Wire) EmitOverlayApp(appID string, running, onAir *bool) {
	if appID == "" || (running == nil && onAir == nil) {
		return
	}
	patches := map[string]any{}

	w.overlayMu.Lock()
	if w.overlay == nil {
		w.overlay = map[string]*overlayState{}
	}
	st := w.overlay[appID]
	if st == nil {
		st = &overlayState{}
		w.overlay[appID] = st
	}
	if running != nil {
		st.running, st.runningSet = *running, true
		patches[overlayLeaf(appID, "running")] = *running
	}
	if onAir != nil {
		st.onAir, st.onAirSet = *onAir, true
		patches[overlayLeaf(appID, "on_air")] = *onAir
	}
	w.overlayMu.Unlock()

	if len(patches) == 0 {
		return
	}
	if sc := w.srv.ActiveScene(); sc != nil {
		_ = sc.Emit(patches)
	}
}

// replayOverlay re-applies every stored stream-level overlay control leaf onto
// the given kit scene — called from SetActive after the kit migrates
// subscribers, so the destination scene carries the state (persistence across
// switch). Only dimensions actually set are emitted.
func (w *Wire) replayOverlay(sceneID string) {
	w.overlayMu.Lock()
	patches := make(map[string]any, len(w.overlay)*2)
	for appID, st := range w.overlay {
		if st.runningSet {
			patches[overlayLeaf(appID, "running")] = st.running
		}
		if st.onAirSet {
			patches[overlayLeaf(appID, "on_air")] = st.onAir
		}
	}
	w.overlayMu.Unlock()
	if len(patches) == 0 {
		return
	}
	if sc, ok := w.srv.Scene(sceneID); ok {
		_ = sc.Emit(patches)
	}
}
