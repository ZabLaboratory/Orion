// Package bluehost is Orion's thin adapter over github.com/ZabLaboratory/Blue/runtime/go
// (ADR-BLUE-012 §4.3/§4.4/§6.6): it owns the preview/on-air instance
// lifecycle and isolation, translating Orion's broadcast vocabulary
// (preview/on-air) to the runtime's portable Mode (Preview/Execute).
// It has no network, storage or Zab dependency of its own — the runtime
// package it wraps is intentionally host-neutral; every Zab-specific
// capability rides through the Providers a caller passes to Prepare/Take.
package bluehost

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// Slot names the two isolated instance roles a Host manages. Per §4.4
// step 6 and invariant §5.10, no on-air effect is ever armed before a
// take is committed — Preview and OnAir are always distinct instances,
// never the same handle re-tagged.
type Slot string

const (
	SlotPreview Slot = "preview"
	SlotOnAir   Slot = "on-air"
)

var (
	ErrAlreadyLoaded = errors.New("bluehost: slot already holds a running instance")
	ErrNotLoaded     = errors.New("bluehost: slot holds no instance")
)

// entry pairs a running instance with the program handle it was started
// from, so Step/Stop never need the caller to keep the handle around.
type entry struct {
	instance *blueruntime.InstanceHandle
	digest   string // scene_digest / program identity this slot is serving
	bundle   []byte // optional LSML render-bundle bytes for this slot, set via SetBundle
}

// Host owns exactly one preview and one on-air instance at a time, per
// §4.4: "Orion canonique charge blue.program.v1 et héberge une instance
// preview Blue isolée en mémoire" / "prépare une instance on-air
// canonique depuis la même référence". A second Prepare/Take on an
// already-loaded slot is refused (ErrAlreadyLoaded) — the caller must
// Release first, keeping "no on-air effect before commit" structurally
// true: there is never a moment with two live on-air instances.
type Host struct {
	mu      sync.Mutex
	runtime *blueruntime.Runtime
	slots   map[Slot]*entry

	// httpEgress/httpRunner wire the async invocation/completion protocol
	// (Blue PR #313, runtime/go effects.go/runtime.go: StepResult.
	// Invocations + Runtime.Complete) to a real outbound HTTP executor for
	// `core.http.request` invocations — see effect_http.go. Both nil by
	// default: every invocation then stays pending, never a silent
	// capability grant. Set once via SetHTTPEffects.
	httpEgress *effects.EgressPolicy
	httpRunner *effects.Runner
	logger     *slog.Logger
}

// NewHost builds an empty Host. One Host per Orion process — it is the
// entire "cycle de vie d'instances longues de blue-runtime-go" §4.3
// grants Orion.
func NewHost() *Host {
	return &Host{
		runtime: blueruntime.NewRuntime(),
		slots:   map[Slot]*entry{},
		logger:  slog.Default(),
	}
}

// modeFor translates Orion's broadcast vocabulary to the portable ABI's
// Mode, per runtime.go's own comment: "Host vocabulary such as 'on-air'
// is intentionally translated to Execute outside this package."
func modeFor(slot Slot) blueruntime.Mode {
	if slot == SlotOnAir {
		return blueruntime.Execute
	}
	return blueruntime.Preview
}

// Prepare loads program and starts a fresh instance in slot, isolated
// from whatever the other slot is running. providers are copied by the
// portable runtime at admission (never read from a DB/catalogue) — the
// caller builds them from Orion's adapters, never from Prism or a client
// payload (§4.4 invariant: no mutable payload initializes an instance).
func (h *Host) Prepare(slot Slot, instanceID, digest string, program []byte, providers []map[string]any, policy blueruntime.CapabilityPolicy) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, exists := h.slots[slot]; exists {
		return fmt.Errorf("%w: %s", ErrAlreadyLoaded, slot)
	}

	handle, err := h.runtime.Load(program)
	if err != nil {
		return fmt.Errorf("bluehost: load %s: %w", slot, err)
	}

	instance, err := h.runtime.Start(handle, blueruntime.StartOptions{
		InstanceID: instanceID,
		Mode:       modeFor(slot),
		Providers:  providers,
		Policy:     policy,
	})
	if err != nil {
		return fmt.Errorf("bluehost: start %s: %w", slot, err)
	}

	h.slots[slot] = &entry{instance: instance, digest: digest}
	return nil
}

// Digest reports the scene_digest the slot is currently serving, or ""
// if the slot is empty — used to short-circuit a redundant re-Prepare on
// the same digest (idempotent re-push, §4.4 does not require it but it
// avoids tearing down a healthy instance for a no-op push).
func (h *Host) Digest(slot Slot) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.slots[slot]
	if !ok {
		return ""
	}
	return e.digest
}

// SetBundle attaches the content-addressed LSML render-bundle bytes to
// slot's current entry, so a caller (the GET render-bundle route) can
// serve back exactly what Prepare/Take last loaded without a second
// Canvas fetch or any Store dependency. No-op if slot is empty — a
// caller races Release only at its own risk, same as every other Host
// method.
func (h *Host) SetBundle(slot Slot, bundle []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, ok := h.slots[slot]; ok {
		e.bundle = bundle
	}
}

// Bundle returns the LSML render-bundle bytes SetBundle last attached to
// slot, or nil if none was set (or the slot is empty).
func (h *Host) Bundle(slot Slot) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.slots[slot]
	if !ok {
		return nil
	}
	return e.bundle
}

// Dispatch delivers an inbound event to the slot's instance.
func (h *Host) Dispatch(slot Slot, data []byte) (blueruntime.Receipt, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.slots[slot]
	if !ok {
		return blueruntime.Receipt{}, fmt.Errorf("%w: %s", ErrNotLoaded, slot)
	}
	return h.runtime.Dispatch(e.instance, data)
}

// Step advances the slot's instance by one deterministic transition. Any
// `core.effect.invoke@1` invocation the transition emitted
// (StepResult.Invocations) is handed to dispatchInvocations AFTER the
// runtime lock is released — Step itself never blocks on network I/O; the
// real HTTP call (when wired via SetHTTPEffects) runs on the worker pool
// and reports back through Runtime.Complete on its own goroutine.
func (h *Host) Step(slot Slot) (blueruntime.StepResult, error) {
	h.mu.Lock()
	e, ok := h.slots[slot]
	if !ok {
		h.mu.Unlock()
		return blueruntime.StepResult{}, fmt.Errorf("%w: %s", ErrNotLoaded, slot)
	}
	instance := e.instance
	result, err := h.runtime.Step(instance)
	h.mu.Unlock()
	if err != nil {
		return result, err
	}
	h.dispatchInvocations(slot, instance, result.Invocations)
	return result, nil
}

// Release stops and forgets the slot's instance. Safe to call on an
// empty slot (no-op) so a take/retake sequence never needs its own
// existence check.
func (h *Host) Release(slot Slot, reason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.slots[slot]
	if !ok {
		return nil
	}
	err := h.runtime.Stop(e.instance, reason)
	delete(h.slots, slot)
	return err
}

// Take prepares a brand-new on-air instance from program and, once it
// starts cleanly, atomically releases whatever on-air instance preceded
// it and commits the new one. Per §4.4/§6.8, the default behaviour is no
// state transfer: the new instance starts cold from the same
// ResolvedSceneRef, never seeded from the outgoing instance's variables.
// A failure before the new instance starts leaves the previous on-air
// instance (if any) completely untouched — the atomicity boundary §4.4
// promises ("un échec avant commit ne modifie pas l'active").
func (h *Host) Take(instanceID, digest string, program []byte, providers []map[string]any, policy blueruntime.CapabilityPolicy) error {
	h.mu.Lock()
	handle, err := h.runtime.Load(program)
	if err != nil {
		h.mu.Unlock()
		return fmt.Errorf("bluehost: load on-air: %w", err)
	}
	instance, err := h.runtime.Start(handle, blueruntime.StartOptions{
		InstanceID: instanceID,
		Mode:       blueruntime.Execute,
		Providers:  providers,
		Policy:     policy,
	})
	if err != nil {
		h.mu.Unlock()
		return fmt.Errorf("bluehost: start on-air: %w", err)
	}

	previous := h.slots[SlotOnAir]
	h.slots[SlotOnAir] = &entry{instance: instance, digest: digest}
	h.mu.Unlock()

	if previous != nil {
		// Compensation for the outgoing instance happens AFTER the new one
		// is committed — a Stop failure here is logged by the caller, never
		// allowed to roll back the take that already succeeded (§4.4: "un
		// échec après commit produit un état typé et une compensation").
		return h.runtime.Stop(previous.instance, "superseded-by-take")
	}
	return nil
}
