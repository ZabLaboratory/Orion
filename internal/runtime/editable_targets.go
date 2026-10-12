package runtime

import (
	"context"
	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// EditableHead identifies one currently active actor. Its writer ID survives a
// Take and changes on structural replacement; retired actors have no read path.
type EditableHead struct {
	SceneID      string `json:"scene_id"`
	SceneVersion string `json:"scene_version"`
	EditSeq      uint64 `json:"edit_seq"`
	WriterID     string `json:"writer_id"`
}

type EditableSnapshot struct {
	Head      EditableHead
	roleEpoch uint64
}

func (p *PreviewSlot) SetEditableProgramWire(wire PreviewWire) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.programWire = wire
}

func (p *PreviewSlot) editableTargetLocked(target string) *previewClone {
	if target == "program" {
		return p.airCurrent
	}
	if target == "preview" {
		return p.current
	}
	return nil
}

func editableHead(cur *previewClone) EditableHead {
	if cur == nil {
		return EditableHead{}
	}
	return EditableHead{cur.sceneID, cur.sceneVersion, cur.editSeq, cur.writerID}
}

func (p *PreviewSlot) EditableTargetHead(target string) (EditableHead, bool) {
	snapshot, editable := p.EditableTargetSnapshot(target)
	return snapshot.Head, editable
}

func (p *PreviewSlot) EditableTargetSnapshot(target string) (EditableSnapshot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.editableTargetLocked(target)
	return EditableSnapshot{editableHead(cur), p.roleEpoch}, cur != nil && cur.editable
}

// ReplaceEditableTarget fences compilation against the actor and sequence read
// before it started. The Host admission callback and native mirror replacement
// run while roles are pinned, so a Take or hot edit cannot redirect the snapshot.
func (p *PreviewSlot) ReplaceEditableTarget(target, sceneID, writerID string, baseSeq *uint64, expected EditableSnapshot, graph *compiler.Graph, bundle *compiler.RenderBundle, editSeq uint64, source []byte, before func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.editableTargetLocked(target)
	if target != "preview" && target != "program" {
		return ErrPreviewSceneMismatch
	}
	if editableHead(cur) != expected.Head || p.roleEpoch != expected.roleEpoch {
		return ErrPreviewEditSequence
	}
	if target == "program" {
		if cur == nil || cur.sceneID != sceneID {
			return ErrPreviewSceneMismatch
		}
		if !cur.editable || p.programWire == nil {
			return ErrPreviewNotEditable
		}
		if writerID == "" || baseSeq == nil {
			return ErrPreviewEditSequence
		}
	}
	if writerID != "" && (cur == nil || cur.writerID != writerID) {
		return ErrPreviewEditSequence
	}
	if baseSeq != nil && (cur == nil || cur.editSeq != *baseSeq) {
		return ErrPreviewEditSequence
	}
	if cur != nil && cur.sceneID == sceneID && editSeq < cur.editSeq {
		return ErrPreviewEditSequence
	}
	if before != nil {
		if err := before(); err != nil {
			return err
		}
	}
	p.activateLocked(target, sceneID, graph, bundle, true, editSeq, source, nil)
	return nil
}

func (p *PreviewSlot) FlushEditableTarget(ctx context.Context, target string) error {
	p.mu.Lock()
	cur := p.editableTargetLocked(target)
	p.mu.Unlock()
	if cur == nil {
		return nil
	}
	return cur.scene.Flush(ctx)
}

func (p *PreviewSlot) EditableTargetMirror(target string) SceneMirror {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.editableTargetLocked(target)
	if cur == nil || cur.mirror == nil {
		return nil
	}
	cur.mirror.mu.RLock()
	defer cur.mirror.mu.RUnlock()
	if len(cur.mirror.mirrors) == 0 {
		return nil
	}
	return cur.mirror.mirrors[0]
}
