package main

import (
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
)

func platformEventTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestStatelessPlatformEventSinkProjectsPreviewAndOnAir(t *testing.T) {
	leaf := "__inputs.platform.twitch.g2nmathias.last_chat"
	payload := map[string]any{"message": "hello", "user": "cosmoselements"}
	writes := make([]bluehost.Slot, 0, 2)
	forwarded := make(map[bluehost.Slot]bluewire.StepResult)

	sink := newStatelessPlatformEventSink(
		func(slot bluehost.Slot, gotLeaf string, gotPayload any) (blueruntime.StepResult, error) {
			if gotLeaf != leaf || !reflect.DeepEqual(gotPayload, payload) {
				t.Fatalf("write input = (%q, %#v), want (%q, %#v)", gotLeaf, gotPayload, leaf, payload)
			}
			writes = append(writes, slot)
			sequence := uint64(len(writes))
			return blueruntime.StepResult{
				RuntimeSequence: sequence,
				Outputs: map[string]any{
					"chat.rows.0.author":  "cosmoselements",
					"chat.rows.0.message": "hello",
				},
			}, nil
		},
		func(slot bluehost.Slot, result bluewire.StepResult) error {
			forwarded[slot] = result
			return nil
		},
		platformEventTestLogger(),
	)
	sink(leaf, payload)

	wantSlots := []bluehost.Slot{bluehost.SlotPreview, bluehost.SlotOnAir}
	if !reflect.DeepEqual(writes, wantSlots) {
		t.Fatalf("write slots = %#v, want %#v", writes, wantSlots)
	}
	for index, slot := range wantSlots {
		got, ok := forwarded[slot]
		if !ok {
			t.Fatalf("slot %s was executed but not projected", slot)
		}
		if got.RuntimeSequence != uint64(index+1) || got.Outputs["chat.rows.0.message"] != "hello" {
			t.Fatalf("slot %s forwarded result = %#v", slot, got)
		}
	}
}

func TestStatelessPlatformEventSinkKeepsSlotsIndependent(t *testing.T) {
	forwarded := make([]bluehost.Slot, 0, 1)
	sink := newStatelessPlatformEventSink(
		func(slot bluehost.Slot, _ string, _ any) (blueruntime.StepResult, error) {
			if slot == bluehost.SlotPreview {
				return blueruntime.StepResult{}, errors.New("preview execution failed")
			}
			return blueruntime.StepResult{RuntimeSequence: 7, Outputs: map[string]any{"ok": true}}, nil
		},
		func(slot bluehost.Slot, _ bluewire.StepResult) error {
			forwarded = append(forwarded, slot)
			return nil
		},
		platformEventTestLogger(),
	)

	sink("__inputs.platform.twitch.g2nmathias.last_chat", map[string]any{"message": "hello"})
	if !reflect.DeepEqual(forwarded, []bluehost.Slot{bluehost.SlotOnAir}) {
		t.Fatalf("forwarded slots = %#v, want on-air despite preview failure", forwarded)
	}
}
