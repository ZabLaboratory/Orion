package lsdp

// Stream-level overlay-app control mirror (ADR 016 Prism §3.2, issue #283).
//
// A `core.overlay-app.set@1` node, used in a stream-level rule (ADR 009),
// sets the desired `{running, on_air}` state of an operator-declared overlay
// app at STREAM level — above any single scene, so it survives scene switches
// (the overlay-app control is not a property of one scene of the show).
//
// # Wire channel — the `overlay_apps` show-level frame (lumencast-go v0.2.0)
//
// The state is carried to the Prism consumer (#360) as the additive,
// show-level `overlay_apps` frame (protocol.OverlayApps), NOT as scene leaves.
// The frame mirrors `scene_roster`: the server caches the complete state, fans
// it out to every live 1.1 subscriber on change, and replays it to each new
// subscriber after its snapshot. Being show-level, it is deliverable even when
// NO scene is active — the reason the old `__overlay.<app_id>.*` scene-riding
// leaves (which required an active scene to Emit onto, and a replay at every
// SetActive) were retired: the kit now owns caching + replay + fan-out.
//
// # Partial updates
//
// The node's `running` / `on_air` are both OPTIONAL: a set may carry only one
// dimension (e.g. reveal on air without touching the process). Each dimension
// is accumulated independently; the emitted frame carries, per app, only the
// dimension(s) ever set (a nil pointer means "never set" — the consumer leaves
// it unchanged).
//
// # Derived cache / persistence
//
// The Wire holds the accumulated per-app state (the derived cache; memory only
// — no DB, no goose: restart loses it, the durable state belongs to the app
// itself, ADR 016 RC #11). On every set it rebuilds the COMPLETE snapshot and
// hands it to the kit's SetOverlayApps, which owns the show-level durability
// (cache + replay-on-join). No scene switch handling is needed here anymore.

import "github.com/Lumencast/lumencast-go/protocol"

// overlayState is the accumulated per-app control state (the derived cache).
// The *Set flags distinguish "never set" from "set to false", so the emitted
// frame carries only the dimensions an operator actually touched.
type overlayState struct {
	running    bool
	runningSet bool
	onAir      bool
	onAirSet   bool
}

// EmitOverlayApp records the stream-level overlay control state and publishes
// the complete show-level overlay_apps snapshot on the kit. running / on_air
// are optional (nil = leave that dimension unchanged) so a set can touch one
// dimension without clobbering the other. Unlike the old leaf channel it needs
// NO active scene: the kit caches the snapshot and replays it on join, so a
// consumer with no scene (Marker) still receives it. Safe for concurrent use.
func (w *Wire) EmitOverlayApp(appID string, running, onAir *bool) {
	if appID == "" || (running == nil && onAir == nil) {
		return
	}

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
	}
	if onAir != nil {
		st.onAir, st.onAirSet = *onAir, true
	}
	snapshot := w.overlaySnapshotLocked()
	w.overlayMu.Unlock()

	w.srv.SetOverlayApps(snapshot)
}

// overlaySnapshotLocked builds the complete per-app control snapshot for the
// overlay_apps frame. Only dimensions actually set become non-nil pointers, so
// the frame preserves the partial-update semantics. Caller MUST hold overlayMu.
func (w *Wire) overlaySnapshotLocked() map[string]protocol.OverlayAppState {
	apps := make(map[string]protocol.OverlayAppState, len(w.overlay))
	for appID, st := range w.overlay {
		var a protocol.OverlayAppState
		if st.runningSet {
			v := st.running
			a.Running = &v
		}
		if st.onAirSet {
			v := st.onAir
			a.OnAir = &v
		}
		apps[appID] = a
	}
	return apps
}
