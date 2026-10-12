package lsdpreception

import (
	"context"
	"encoding/json"
	"errors"
	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
	"time"
)

func (r *Reception) Read(ctx context.Context, target string) (any, error) {
	client, err := native.Dial(ctx, r.Address, "", nil)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	raw, err := client.Exchange(ctx, map[string]any{"kind": "state.read", "target": target})
	if err != nil {
		return nil, err
	}
	var snapshot struct {
		Target string
		State  any
	}
	err = json.Unmarshal(raw, &snapshot)
	if snapshot.Target != target {
		return nil, errors.New("NATIVE_CONTROL_TARGET_INVALID")
	}
	return snapshot.State, err
}

// WriteLeaf preserves every other writer's fields and rebases only after a
// proven BASE_MISMATCH rejection. The operation is an absolute assignment.
func (r *Reception) WriteLeaf(ctx context.Context, target, path string, value any) error {
	return r.WriteLeaves(ctx, target, []any{map[string]any{"op": "add", "path": path, "value": value}})
}

// WriteLeaves applies related absolute observations in one native transaction.
func (r *Reception) WriteLeaves(ctx context.Context, target string, operations []any) error {
	return r.WriteLeavesFromState(ctx, target, func(any) ([]any, error) { return operations, nil })
}

// WriteLeavesFromState rebases state-dependent operations only after a proven
// BASE_MISMATCH refusal. The builder is pure and may be called again.
func (r *Reception) WriteLeavesFromState(ctx context.Context, target string, build func(any) ([]any, error)) error {
	for attempt := 0; attempt < 8; attempt++ {
		before, err := r.Read(ctx, target)
		if err != nil {
			return err
		}
		operations, err := build(before)
		if err != nil {
			return err
		}
		client, err := native.Dial(ctx, r.Address, "", nil)
		if err != nil {
			return err
		}
		id, err := native.NewID()
		if err != nil {
			client.Close()
			return err
		}
		raw, err := client.Exchange(ctx, map[string]any{"format": "lsdp.apply/1", "id": id, "target": target, "beforeHash": treeHash(before), "operations": operations, "require": "applied"})
		client.Close()
		if err == nil {
			var receipt struct{ Level, Target, TransactionID string }
			if json.Unmarshal(raw, &receipt) != nil || receipt.Level != "applied" || receipt.Target != target || receipt.TransactionID != id {
				return errors.New("NATIVE_CONTROL_NOT_APPLIED")
			}
			return nil
		}
		var wireError *native.WireError
		if !errors.As(err, &wireError) || wireError.Code != "BASE_MISMATCH" {
			return err
		}
	}
	return errors.New("NATIVE_CONTROL_BUSY")
}

// Watch reconnects with a fresh authoritative snapshot. No polling of a large
// state document and no unbounded event queue: callback is synchronous.
func (r *Reception) Watch(ctx context.Context, target string, onState func(any), onError func(error)) {
	for ctx.Err() == nil {
		err := r.watchOnce(ctx, target, onState)
		if ctx.Err() != nil {
			return
		}
		onError(err)
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (r *Reception) watchOnce(ctx context.Context, target string, onState func(any)) error {
	client, err := native.Dial(ctx, r.Address, "", nil)
	if err != nil {
		return err
	}
	defer client.Close()
	raw, err := client.Exchange(ctx, map[string]any{"kind": "state.read", "target": target})
	if err != nil {
		return err
	}
	var snapshot struct{ State any }
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		return err
	}
	state := snapshot.State
	hash := treeHash(state)
	onState(state)
	if err = client.Subscribe(ctx, target, hash); err != nil {
		return err
	}
	var sequence uint64
	for ctx.Err() == nil {
		err = client.Next(ctx, target, func(raw json.RawMessage) error {
			var event struct {
				Kind     string
				Sequence uint64
				Mutation struct {
					Format, ID, Target, BeforeHash, AfterHash string
					Operations                                []map[string]any
				}
				Receipt struct{ Target, TransactionID, StateHash string }
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				return err
			}
			application := event.Kind == "applied_change"
			if !application && event.Kind != "change" || event.Sequence <= sequence || event.Mutation.Target != target || event.Receipt.Target != target || event.Receipt.TransactionID != event.Mutation.ID || event.Mutation.BeforeHash != hash {
				return errors.New("NATIVE_CONTROL_RESYNC_REQUIRED")
			}
			format := "lsdp.tree/1"
			if application {
				format = "lsdp.apply/1"
			}
			if event.Mutation.Format != format {
				return errors.New("NATIVE_CONTROL_FORMAT_INVALID")
			}
			next, err := applyObjectOperations(state, event.Mutation.Operations)
			if err != nil {
				return err
			}
			nextHash := treeHash(next)
			if !application && (nextHash != event.Mutation.AfterHash || nextHash != event.Receipt.StateHash) {
				return errors.New("NATIVE_CONTROL_INTEGRITY_FAILED")
			}
			state = next
			hash = nextHash
			sequence = event.Sequence
			onState(state)
			return nil
		})
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (r *Reception) Replace(ctx context.Context, target string, document any) error {
	// Full assignments are idempotent. Retry their identical identity/payload
	// across a receiver restart, bounded by the caller's deadline.
	id, err := native.NewID()
	if err != nil {
		return err
	}
	completed := false
	for ctx.Err() == nil {
		completed, err = r.replaceOnce(ctx, id, target, document, completed)
		if err == nil {
			return nil
		}
		var wireError *native.WireError
		if errors.As(err, &wireError) {
			return err
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}
func (r *Reception) replaceOnce(ctx context.Context, id, target string, document any, completed bool) (bool, error) {
	client, err := native.Dial(ctx, r.Address, "", nil)
	if err != nil {
		return completed, err
	}
	defer client.Close()
	kind := "orion.state/1"
	if isSolarSceneTarget(target) {
		kind = "solar.lsml/1"
	}
	if !completed {
		raw, err := client.SendPort(ctx, id, target, kind, document)
		if err != nil {
			return false, err
		}
		if err = completedRoute(raw, id); err != nil {
			return false, err
		}
		completed = true
	}
	// After the route ACK is known, a lost release receipt can retry only the
	// release. Replaying a forgotten full assignment could overwrite a newer edit.
	return completed, forgetCompletedRoute(func(request any) (json.RawMessage, error) {
		return client.Exchange(ctx, request)
	}, id)
}

func completedRoute(raw json.RawMessage, id string) error {
	if err := native.Completed(raw); err != nil {
		return err
	}
	var receipt struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &receipt) != nil || receipt.ID != id {
		return errors.New("NATIVE_ROUTE_RECEIPT_ID_INVALID")
	}
	return nil
}

// Routing retains full payload/packet memory for replay until its owner
// explicitly releases the record. Only call after this exact route completed.
func forgetCompletedRoute(exchange func(any) (json.RawMessage, error), id string) error {
	raw, err := exchange(map[string]any{"kind": "route.forget", "id": id, "force": false})
	if err != nil {
		return err
	}
	var receipt struct{ ID, Status string }
	if json.Unmarshal(raw, &receipt) != nil ||
		(receipt.Status != "unknown" && (receipt.Status != "forgotten" || receipt.ID != id)) {
		return errors.New("NATIVE_ROUTE_RELEASE_NOT_ACKNOWLEDGED")
	}
	return nil
}
