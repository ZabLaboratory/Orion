package lsdp

// Stream-level slot assignment mirror (ADR Blue 009 §3.3, issue #260).
//
// A `zabcam.assign-slot@1` node, after a durable ZabCam upsert, binds
// `slot_ref → peer_label` at STREAM level — above any single scene, so it
// survives scene switches (a `meet-peer` slot of any scene of the stream
// resolves to the bound peer). The binding is carried to Solar as an LSDP
// leaf keyed by slot_ref so Solar re-keys the rendered `meet.peer` instantly.
//
// # Wire leaf format (consumed by Solar #28)
//
//	__cam.slots.<slot_ref> : "<peer_label>"   (string scalar leaf)
//
// The `__cam.slots.` reserved prefix is NOT a scene leaf: it rides the wire
// directly via the kit scene (Set/Emit), bypassing the per-scene bound-leaf
// gate (sceneMirror.Forward) which governs only the reactive scene tap. A
// peer_label is a string, so it passes the §3.2.1 scalar shape contract.
//
// # Derived cache / persistence
//
// The Wire holds the authoritative-for-LSDP map (the derived cache; ZabCam is
// the durable authority). On a scene switch the kit migrates subscribers to
// the destination scene with that scene's snapshot — which does NOT carry the
// stream-level slots — so SetActive replays the whole map onto the new active
// scene, re-keying every slot. A late joiner therefore sees the slots in the
// destination scene's accumulated state.

const slotLeafPrefix = "__cam.slots."

// EmitSlotAssignment records the stream-level binding and emits the re-keying
// delta on the active kit scene. With no active scene yet, it only stores —
// the next SetActive replays it. Safe for concurrent use (the assign-slot op
// runs on a scene goroutine; SetActive on the show's).
func (w *Wire) EmitSlotAssignment(slotRef, peerLabel string) {
	if slotRef == "" {
		return
	}
	w.slotMu.Lock()
	if w.slots == nil {
		w.slots = map[string]string{}
	}
	w.slots[slotRef] = peerLabel
	w.slotMu.Unlock()

	if sc := w.srv.ActiveScene(); sc != nil {
		_ = sc.Emit(map[string]any{slotLeafPrefix + slotRef: peerLabel})
	}
}

// replaySlots re-applies every stored stream-level slot binding onto the
// given kit scene — called from SetActive after the kit migrates subscribers,
// so the destination scene carries the bindings (persistence across switch).
func (w *Wire) replaySlots(sceneID string) {
	w.slotMu.Lock()
	patches := make(map[string]any, len(w.slots))
	for ref, label := range w.slots {
		patches[slotLeafPrefix+ref] = label
	}
	w.slotMu.Unlock()
	if len(patches) == 0 {
		return
	}
	if sc, ok := w.srv.Scene(sceneID); ok {
		_ = sc.Emit(patches)
	}
}
