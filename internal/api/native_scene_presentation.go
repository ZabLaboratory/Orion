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
	timings                            map[string]time.Duration
}

func (p *nativeScenePresentation) PresentationTimings() map[string]time.Duration {
	return p.timings
}

func NativeScenePresenter(reception *lsdpreception.Reception, sourceFor func(bluehost.Slot, string, string, []byte) (map[string]any, error), targetFor ...func(bluehost.Slot) string) ScenePresenter {
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
		started := time.Now()
		target := "solar/" + lane
		if len(targetFor) > 0 {
			target = targetFor[0](slot)
		}
		previous, err := reception.Read(ctx, target)
		if err != nil {
			return nil, err
		}
		presentation := &nativeScenePresentation{reception: reception, lane: lane, target: target, id: id, sceneID: sceneID, version: sourceVersion, previous: previous, source: document,
			timings: map[string]time.Duration{"solarread": time.Since(started)}}
		return presentation, presentation.phase(ctx, "prepare", "prepared")
	}
}
func (p *nativeScenePresentation) phase(parent context.Context, phase, expected string) error {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	base := p.previous
	if base == nil || phase == "commit" || phase == "finalize" {
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
	transition := map[string]any{"request_id": p.id, "phase": phase, "lane": p.lane}
	// Only prepare needs two sources: the accepted frame and its hidden candidate.
	// Commit/finalize carry the candidate once at the resource root, which also
	// permits a fresh renderer to reconstruct that authoritative stage.
	if phase == "prepare" {
		transition["source"] = p.source
	}
	document["x-solar-transition"] = transition
	// Watch accepted state before publishing: no interval quantization, polling
	// connections or lost fast feedback. Reception owns integrity and resync.
	outcome := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.reception.Watch(ctx, "orion/state", func(state any) {
			matched, err := presentationAcknowledgment(state, p.lane, p.id, phase, expected, p.sceneID, p.version)
			if matched {
				select {
				case outcome <- err:
				default:
				}
			}
		}, func(error) { /* Watch rereads and reconnects within the phase deadline. */ })
	}()
	defer func() { cancel(); <-done }()
	started := time.Now()
	if err = p.reception.Replace(ctx, p.target, document); err != nil {
		return err
	}
	p.timings["solar"+phase+"replace"] += time.Since(started)
	started = time.Now()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-outcome:
			p.timings["solar"+phase+"wait"] += time.Since(started)
			return err
		case <-ctx.Done():
			return fmt.Errorf("SOLAR_ACK_%s: %w", expected, ctx.Err())
		case <-ticker.C:
			// Reassert only the absolute phase after a receiver restart. Blue
			// execution and relative mutations are never replayed here.
			if err := p.reception.Replace(ctx, p.target, document); err != nil {
				return err
			}
		}
	}
}

func presentationAcknowledgment(state any, lane, id, phase, expected, sceneID, version string) (bool, error) {
	root, _ := state.(map[string]any)
	ack, _ := root["solar_presentation_"+lane].(map[string]any)
	if ack["request_id"] != id {
		return false, nil
	}
	if ack["phase"] == "failed" && ack["transition_phase"] == phase {
		return true, fmt.Errorf("SOLAR_PRESENTATION_REJECTED: %v", ack["error"])
	}
	return ack["phase"] == expected && (expected == "aborted" || ack["scene_id"] == sceneID && ack["scene_version"] == version), nil
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
	started := time.Now()
	err := p.reception.Replace(ctx, p.target, p.source)
	p.timings["solarcleanup"] += time.Since(started)
	return err
}
func (p *nativeScenePresentation) Abort(ctx context.Context) error {
	if err := p.phase(ctx, "abort", "aborted"); err != nil {
		return err
	}
	return p.reception.Replace(ctx, p.target, p.previous)
}
