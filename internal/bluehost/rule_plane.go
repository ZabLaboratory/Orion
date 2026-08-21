package bluehost

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

// ErrRuleNotActive reports that a caller addressed no promoted rule.
var ErrRuleNotActive = errors.New("bluehost: stream rule is not active")

// RuleContract is the declared operator surface of one promoted stream rule.
// RuleID is the blueprint id used by the unchanged ?rule= selector contract.
type RuleContract struct {
	RuleID   string
	Triggers []TriggerDecl
	Awaits   []AwaitDecl
}

type ruleInstance struct {
	host   *Host
	digest string
}

// RulePlane owns the process-local stream-rule runtime. It is deliberately
// orthogonal to Host's preview/on-air scene slots: a promoted rule has one
// long-lived Engine B instance whose effects apply to the show-level overlay
// plane, regardless of which scene occupies either slot. The SlotOnAir used
// inside each private child Host is only the portable Execute-mode adapter;
// it is not a third scene slot and is never exposed through the scene API.
//
// The plane has no durable store. Prism owns intent and replays it after an
// Orion restart; Orion owns only reconstructible runtime state and therefore
// remains stateless in the platform sense.
type RulePlane struct {
	mu           sync.RWMutex
	rules        map[string]*ruleInstance
	providers    []map[string]any
	policy       blueruntime.CapabilityPolicy
	effects      EffectDeps
	logger       *slog.Logger
	showEmitSink ShowEmitSink

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
	interval time.Duration
}

// NewRulePlane constructs and starts a volatile stream-rule plane. tickHz is
// shared with the scene runtime so on-tick/delay entrypoints advance at the
// same cadence. A non-positive value uses the production default of 60 Hz.
func NewRulePlane(providers []map[string]any, policy blueruntime.CapabilityPolicy, effects EffectDeps, tickHz int, logger *slog.Logger) *RulePlane {
	if tickHz <= 0 {
		tickHz = 60
	}
	if logger == nil {
		logger = slog.Default()
	}
	p := &RulePlane{
		rules:     map[string]*ruleInstance{},
		providers: providers,
		policy:    policy,
		effects:   effects,
		logger:    logger,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		interval:  time.Second / time.Duration(tickHz),
	}
	go p.run()
	return p
}

func (p *RulePlane) run() {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	defer close(p.done)
	for {
		select {
		case <-ticker.C:
			p.TickAll(p.interval.Seconds())
		case <-p.stop:
			return
		}
	}
}

// Stop releases every volatile rule instance and terminates the ticker.
func (p *RulePlane) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done

	p.mu.Lock()
	rules := p.rules
	p.rules = map[string]*ruleInstance{}
	p.mu.Unlock()
	for _, rule := range rules {
		if err := rule.host.Release(SlotOnAir, "stream-rule-plane-stop"); err != nil {
			p.logger.Warn("stream rule release failed", "err", err)
		}
	}
}

// Promote loads a published blue.program.v1 as a global stream rule. The
// operation is idempotent for the same (ruleID,digest). A changed digest is
// prepared and started before it atomically replaces the previous instance.
func (p *RulePlane) Promote(ruleID, digest string, program []byte) error {
	if p == nil {
		return errors.New("bluehost: nil stream rule plane")
	}
	if ruleID == "" || digest == "" || len(program) == 0 {
		return errors.New("bluehost: stream rule requires id, digest and program")
	}

	p.mu.RLock()
	current := p.rules[ruleID]
	if current != nil && current.digest == digest {
		p.mu.RUnlock()
		return nil
	}
	p.mu.RUnlock()

	host := NewHost()
	host.SetHTTPEffects(p.effects, p.logger)
	host.SetOverlayMirror(p.effects.OverlayMirror)
	host.SetShowEmitSink(p.emitShowEvent)
	if err := host.Take(
		"stream-rule:"+ruleID,
		ruleID,
		digest,
		program,
		p.providers,
		p.policy,
		NewEffectHandlers(p.effects, blueruntime.Execute),
	); err != nil {
		return fmt.Errorf("bluehost: promote stream rule %s: %w", ruleID, err)
	}
	if _, err := host.Step(SlotOnAir); err != nil {
		_ = host.Release(SlotOnAir, "stream-rule-start-failed")
		return fmt.Errorf("bluehost: start stream rule %s: %w", ruleID, err)
	}

	p.mu.Lock()
	current = p.rules[ruleID]
	if current != nil && current.digest == digest {
		p.mu.Unlock()
		_ = host.Release(SlotOnAir, "stream-rule-duplicate")
		return nil
	}
	p.rules[ruleID] = &ruleInstance{host: host, digest: digest}
	p.mu.Unlock()

	if current != nil {
		if err := current.host.Release(SlotOnAir, "stream-rule-superseded"); err != nil {
			p.logger.Warn("superseded stream rule release failed", "rule_id", ruleID, "err", err)
		}
	}
	return nil
}

// SetShowEmitSink wires the stream rule's core.show.emit@1 output to the
// embedding's active scene-intent targets. Existing rules are updated and
// future promotions inherit the same seam. The rule plane remains stateless;
// the caller owns which slots are active and how the event is admitted.
func (p *RulePlane) SetShowEmitSink(sink ShowEmitSink) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.showEmitSink = sink
	hosts := make([]*Host, 0, len(p.rules))
	for _, rule := range p.rules {
		hosts = append(hosts, rule.host)
	}
	p.mu.Unlock()
	for _, host := range hosts {
		host.SetShowEmitSink(p.emitShowEvent)
	}
}

func (p *RulePlane) emitShowEvent(topic string, payload any) {
	if p == nil {
		return
	}
	p.mu.RLock()
	sink := p.showEmitSink
	p.mu.RUnlock()
	if sink != nil {
		sink(topic, payload)
	}
}

// Demote removes a rule idempotently. Runtime state is not persisted.
func (p *RulePlane) Demote(ruleID string) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	rule := p.rules[ruleID]
	delete(p.rules, ruleID)
	p.mu.Unlock()
	if rule == nil {
		return nil
	}
	return rule.host.Release(SlotOnAir, "stream-rule-demoted")
}

// IDs returns the active rule ids in deterministic order.
func (p *RulePlane) IDs() []string {
	if p == nil {
		return []string{}
	}
	p.mu.RLock()
	ids := make([]string, 0, len(p.rules))
	for id := range p.rules {
		ids = append(ids, id)
	}
	p.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

// Digest returns the running program digest, or "" when the rule is absent.
func (p *RulePlane) Digest(ruleID string) string {
	if p == nil {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if rule := p.rules[ruleID]; rule != nil {
		return rule.digest
	}
	return ""
}

// Contracts returns every promoted rule's declared operator contract sorted
// by rule id. Returned slices are copied from the child host's immutable
// metadata so callers cannot mutate live state.
func (p *RulePlane) Contracts() []RuleContract {
	ids := p.IDs()
	out := make([]RuleContract, 0, len(ids))
	for _, id := range ids {
		p.mu.RLock()
		rule := p.rules[id]
		if rule == nil {
			p.mu.RUnlock()
			continue
		}
		triggers, awaits := rule.host.DeclaredContracts(SlotOnAir)
		p.mu.RUnlock()
		out = append(out, RuleContract{
			RuleID:   id,
			Triggers: append([]TriggerDecl(nil), triggers...),
			Awaits:   append([]AwaitDecl(nil), awaits...),
		})
	}
	return out
}

// HasTrigger fails closed for an absent rule or entrypoint.
func (p *RulePlane) HasTrigger(ruleID, callID string) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	rule := p.rules[ruleID]
	return rule != nil && rule.host.HasTrigger(SlotOnAir, callID)
}

// Call fires one global rule entrypoint.
func (p *RulePlane) Call(ruleID, callID string, payload any) (blueruntime.StepResult, error) {
	if p == nil {
		return blueruntime.StepResult{}, ErrRuleNotActive
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	rule := p.rules[ruleID]
	if rule == nil {
		return blueruntime.StepResult{}, ErrRuleNotActive
	}
	return rule.host.Call(SlotOnAir, callID, payload)
}

// PendingAwaitNames exposes the live await registry of one global rule.
func (p *RulePlane) PendingAwaitNames(ruleID string) []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	rule := p.rules[ruleID]
	if rule == nil {
		return nil
	}
	return rule.host.PendingAwaitNames(SlotOnAir)
}

// Resolve resumes a parked await on one global rule.
func (p *RulePlane) Resolve(ruleID, awaitName string, value any) (blueruntime.StepResult, error) {
	if p == nil {
		return blueruntime.StepResult{}, ErrRuleNotActive
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	rule := p.rules[ruleID]
	if rule == nil {
		return blueruntime.StepResult{}, ErrRuleNotActive
	}
	return rule.host.Resolve(SlotOnAir, awaitName, value)
}

// WritePlatformEvent fans one canonical platform leaf to every global rule.
// The plane is independent of scene membership, so a scene flip cannot alter
// this target set. Callers retain admission/order ownership.
func (p *RulePlane) WritePlatformEvent(leaf string, payload any) {
	if p == nil {
		return
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for id, rule := range p.rules {
		if _, err := rule.host.WritePlatformEvent(SlotOnAir, leaf, payload); err != nil {
			p.logger.Warn("stream rule platform event failed", "rule_id", id, "err", err)
		}
	}
}

// TickAll advances timers for all global rules. Errors are isolated per rule:
// one faulty rule cannot stop the plane or the scene tick.
func (p *RulePlane) TickAll(deltaSeconds float64) {
	if p == nil {
		return
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for id, rule := range p.rules {
		if _, err := rule.host.Tick(SlotOnAir, deltaSeconds); err != nil && !errors.Is(err, ErrNotLoaded) {
			p.logger.Warn("stream rule tick failed", "rule_id", id, "err", err)
		}
	}
}
