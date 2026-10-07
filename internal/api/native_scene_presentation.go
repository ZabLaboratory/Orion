package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
	"strings"
	"time"
)

type nativeScenePresentation struct {
	reception                          *lsdpreception.Reception
	lane, target, id, sceneID, version string
	previous                           any
	source                             map[string]any
}

func NativeScenePresenter(reception *lsdpreception.Reception, sourceFor func(bluehost.Slot, string, string, []byte) (map[string]any, error)) ScenePresenter {
	return func(ctx context.Context, slot bluehost.Slot, id, sceneID, version string, source []byte) (ScenePresentation, error) {
		lane := "preview"
		if slot == bluehost.SlotOnAir {
			lane = "program"
		}
		if id == "" || len(id) > 128 || strings.ContainsAny(id, "/\\ .") {
			return nil, errors.New("SOLAR_REQUEST_ID_INVALID")
		}
		document, err := sourceFor(slot, sceneID, version, source)
		if err != nil {
			return nil, err
		}
		sourceVersion, ok := document["scene_version"].(string)
		if !ok || sourceVersion == "" || document["scene_id"] != sceneID {
			return nil, errors.New("SOLAR_SOURCE_IDENTITY_INVALID")
		}
		previous, err := reception.Read(ctx, "solar/"+lane)
		if err != nil {
			return nil, err
		}
		presentation := &nativeScenePresentation{reception: reception, lane: lane, target: "solar/" + lane, id: id, sceneID: sceneID, version: sourceVersion, previous: previous, source: document}
		return presentation, presentation.phase(ctx, "prepare", "prepared")
	}
}
func (p *nativeScenePresentation) phase(parent context.Context, phase, expected string) error {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	base := p.previous
	if base == nil || phase == "finalize" {
		base = p.source
	}
	raw, err := json.Marshal(base)
	if err != nil {
		return err
	}
	var document map[string]any
	if err = json.Unmarshal(raw, &document); err != nil {
		return err
	}
	document["x-solar-transition"] = map[string]any{"request_id": p.id, "phase": phase, "source": p.source}
	if err = p.reception.Replace(ctx, p.target, document); err != nil {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	lastWrite := time.Now()
	for {
		state, err := p.reception.Read(ctx, "orion/state")
		if err == nil {
			root, _ := state.(map[string]any)
			ack, _ := root["solar_presentation_"+p.lane].(map[string]any)
			if ack["request_id"] == p.id {
				if ack["phase"] == "failed" && ack["transition_phase"] == phase {
					return fmt.Errorf("SOLAR_PRESENTATION_REJECTED: %v", ack["error"])
				}
				if ack["phase"] == expected && (expected == "aborted" || ack["scene_id"] == p.sceneID && ack["scene_version"] == p.version) {
					return nil
				}
			}
		}
		// A process restart may precede its parent's RAM checkpoint. Reassert
		// this absolute phase, never replay Blue execution or a relative edit.
		if time.Since(lastWrite) >= time.Second {
			if err := p.reception.Replace(ctx, p.target, document); err != nil {
				return err
			}
			lastWrite = time.Now()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("SOLAR_ACK_%s: %w", expected, ctx.Err())
		case <-ticker.C:
		}
	}
}
func (p *nativeScenePresentation) Commit(ctx context.Context) error {
	return p.phase(ctx, "commit", "committed")
}
func (p *nativeScenePresentation) Finalize(ctx context.Context) error {
	if err := p.phase(ctx, "finalize", "active"); err != nil {
		return err
	}
	// Removing metadata releases Solar's retained compensation frame. Do not
	// open the Blue gate until that absolute assignment has been accepted.
	return p.reception.Replace(ctx, p.target, p.source)
}
func (p *nativeScenePresentation) Abort(ctx context.Context) error {
	if err := p.phase(ctx, "abort", "aborted"); err != nil {
		return err
	}
	return p.reception.Replace(ctx, p.target, p.previous)
}
