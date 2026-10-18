package bluewire

import (
	"context"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// PauseRoleSwap joins both loops while retaining their exact bridge/projection
// identity. finish(true) retargets those same bridges after the host/hub commit;
// finish(false) restores them. It never runs StepOnce or an on-start callback.
func (r *Registry) PauseRoleSwap() func(bool) {
	r.opMu.Lock()
	r.mu.Lock()
	p, v := r.running[bluehost.SlotOnAir], r.running[bluehost.SlotPreview]
	pm, vm := r.scenes[bluehost.SlotOnAir], r.scenes[bluehost.SlotPreview]
	r.running = map[bluehost.Slot]*bridgeRun{}
	r.scenes = map[bluehost.Slot]runtime.SceneMirror{}
	for _, run := range []*bridgeRun{p, v} {
		if run != nil {
			run.cancel()
		}
	}
	r.mu.Unlock()
	for _, run := range []*bridgeRun{p, v} {
		if run != nil {
			<-run.done
		}
	}
	return func(commit bool) {
		if commit {
			p, v = v, p
			pm, vm = vm, pm
		}
		r.mu.Lock()
		for _, item := range []struct {
			slot   bluehost.Slot
			run    *bridgeRun
			mirror runtime.SceneMirror
		}{{bluehost.SlotOnAir, p, pm}, {bluehost.SlotPreview, v, vm}} {
			if item.mirror != nil {
				r.scenes[item.slot] = item.mirror
			}
			if item.run == nil {
				continue
			}
			b := item.run.bridge
			b.slot = item.slot
			if item.slot == bluehost.SlotOnAir {
				b.target = blueproject.TargetProgram
			} else {
				b.target = blueproject.TargetPreview
			}
			ctx, cancel := context.WithCancel(context.Background())
			run := &bridgeRun{cancel: cancel, done: make(chan struct{}), bridge: b, interval: item.run.interval, onError: item.run.onError}
			r.running[item.slot] = run
			go func() { defer close(run.done); b.Run(ctx, run.interval, run.onError) }()
		}
		r.mu.Unlock()
		r.opMu.Unlock()
	}
}
