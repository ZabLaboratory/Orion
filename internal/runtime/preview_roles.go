package runtime

import "errors"

// CurrentMirror is the original native mirror of the current editable actor.
// The registry uses its existing mutation seam for stream-level LSML effects.
func (p *PreviewSlot) CurrentMirror() SceneMirror {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil || p.current.mirror == nil {
		return nil
	}
	f := p.current.mirror
	f.mu.RLock()
	defer f.mu.RUnlock()
	if len(f.mirrors) == 0 {
		return nil
	}
	return f.mirrors[0]
}

// StageRoleSwap pins the two existing editable actors. Their Mirror authority
// is transferred by the Hub; no clone, generation or snapshot is created here.
// finish is required on every path and releases the pin.
func (p *PreviewSlot) StageRoleSwap(sceneID string) (func(bool), error) {
	p.mu.Lock()
	if p.current != nil && (p.current.sceneID != sceneID || !p.current.editable) {
		p.mu.Unlock()
		return nil, errors.New("LANE_COMMIT_EDITABLE_MISMATCH")
	}
	return func(commit bool) {
		if commit {
			p.current, p.airCurrent = p.airCurrent, p.current
			if p.current != nil {
				p.current.airPromoted = false
			}
			if p.airCurrent != nil {
				p.airCurrent.airPromoted = true
			}
		}
		p.mu.Unlock()
	}, nil
}
