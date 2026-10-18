package bluehost

import "encoding/json"

// SetSceneInterface attaches only the operator declaration from admitted LSML.
// It neither compiles nor retains a second copy of the scene/assets. An absent
// declaration in canonical LSML is authoritative: it clears legacy controls.
// Non-LSML compatibility callers continue to use SetBundle.
func (h *Host) SetSceneInterface(slot Slot, source []byte) {
	var document struct {
		LSML           string          `json:"lsml"`
		OperatorInputs json.RawMessage `json:"operator_inputs"`
	}
	if json.Unmarshal(source, &document) != nil || document.LSML == "" {
		return
	}
	inputs := document.OperatorInputs
	if len(inputs) == 0 || string(inputs) == "null" {
		inputs = json.RawMessage(`[]`)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.slots[slot]; e != nil {
		e.operatorInputs = append([]byte(nil), inputs...)
	}
}

// OperatorInputs returns an owned copy of the current slot's declaration.
// Compatibility bundles are consulted only when no LSML interface was attached.
func (h *Host) OperatorInputs(slot Slot) json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.slots[slot]
	if e == nil {
		return nil
	}
	return entryOperatorInputs(e)
}

// SceneOperatorInputs resolves the exact scene/program address atomically,
// with Program precedence when both slots serve the same scene. It also serves
// source-only/static scenes, whose compatibility bundle is deliberately absent.
func (h *Host) SceneOperatorInputs(sceneID, digest string) (json.RawMessage, bool) {
	if sceneID == "" || digest == "" {
		return nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, slot := range []Slot{SlotOnAir, SlotPreview} {
		if e := h.slots[slot]; e != nil && e.sceneID == sceneID && e.digest == digest {
			return entryOperatorInputs(e), true
		}
	}
	return nil, false
}

func entryOperatorInputs(e *entry) json.RawMessage {
	inputs := e.operatorInputs
	if inputs == nil {
		var document struct {
			OperatorInputs json.RawMessage `json:"operator_inputs"`
		}
		if json.Unmarshal(e.bundle, &document) == nil {
			inputs = document.OperatorInputs
		}
	}
	if len(inputs) == 0 {
		return json.RawMessage(`[]`)
	}
	return append(json.RawMessage(nil), inputs...)
}
