package bluehost

import (
	"fmt"
	"sort"
	"strings"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/canonical"
)

const animationBag = "__animation.play"

type SceneMutationSink func(Slot, []map[string]any) error

func (h *Host) SurfaceAvailability() (show, mutation, animation bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.showEmitSink != nil, h.sceneMutationSink != nil, h.sceneMutationSink != nil
}

func (h *Host) SetSceneMutationSink(sink SceneMutationSink) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sceneMutationSink = sink
}

func (h *Host) publishAnimation(slot Slot, id string, request map[string]any) error {
	if id == "" || stringField(request, "animation_id") == "" {
		return fmt.Errorf("ANIMATION_REQUEST_INVALID: animation_id is required")
	}
	h.mu.Lock()
	sink := h.sceneMutationSink
	h.mu.Unlock()
	if sink == nil {
		return fmt.Errorf("EFFECT_PROVIDER_UNAVAILABLE: animation sink is not configured")
	}
	value := map[string]any{}
	for key, item := range request {
		value[key] = item
	}
	if value["command_id"] == nil {
		value["command_id"] = id
	}
	leaf := "__animation." + stringField(request, "animation_id")
	leaf = strings.ReplaceAll(strings.ReplaceAll(leaf, "~", "~0"), "/", "~1")
	return sink(slot, []map[string]any{{"op": "add", "path": "/defaults/" + leaf, "value": value}})
}

func (h *Host) dispatchAnimation(slot Slot, instance *blueruntime.InstanceHandle, variables map[string]any) {
	bag, _ := variables[animationBag].(map[string]any)
	ids := make([]string, 0, len(bag))
	for id := range bag {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		record, _ := bag[id].(map[string]any)
		digest, err := canonical.Digest(record)
		if err != nil {
			continue
		}
		h.mu.Lock()
		e := h.slots[slot]
		if e == nil || e.instance != instance || e.animationSeen[id] == digest {
			h.mu.Unlock()
			continue
		}
		h.mu.Unlock()
		request := map[string]any{"command_id": digest}
		for _, field := range []string{"config", "inputs"} {
			values, _ := record[field].(map[string]any)
			for key, value := range values {
				request[key] = value
			}
		}
		if err := h.publishAnimation(slot, id, request); err != nil {
			h.logger.Error("Blue animation delivery failed", "node", id, "error", err)
			continue
		}
		h.mu.Lock()
		if h.slots[slot] == e {
			if e.animationSeen == nil {
				e.animationSeen = map[string]string{}
			}
			e.animationSeen[id] = digest
		}
		h.mu.Unlock()
	}
}
