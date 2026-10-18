package runtime

// ReleaseNativeProgram detaches its writer before joining the scene loop.
// Caller holds the Program Host lease and has frozen its native mirror.
// Preview and its compilation epoch are deliberately untouched.
func (p *PreviewSlot) ReleaseNativeProgram(expected EditableHead) error {
	p.mu.Lock()
	cur := p.editableTargetLocked("program")
	if editableHead(cur) != expected {
		p.mu.Unlock()
		return ErrPreviewEditSequence
	}
	p.airCurrent = nil
	p.nativeAir = false
	p.mu.Unlock()
	if cur != nil {
		cur.scene.Stop()
	}
	return nil
}
