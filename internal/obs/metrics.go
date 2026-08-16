package obs

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics groups the runtime counters/gauges Orion publishes on the
// internal scrape endpoint. One field per metric so callers reach for
// a typed accessor instead of magic strings.
type Metrics struct {
	Registry *prometheus.Registry

	WSConnections *prometheus.GaugeVec
	WSMessagesIn  *prometheus.CounterVec
	WSMessagesOut *prometheus.CounterVec
	// WSDropped (`orion_ws_dropped_total{scene_id,reason}`) counts a live-
	// show WS fanout that hit a full subscriber queue (ADR-BLUE-012
	// §12/B8, B3-R6-OPS-ORION / Orion#274): "collapse" (the subscriber was
	// answered with a fresh-snapshot reset instead of a silent drop) or
	// "stuck_timeout" (the subscriber stayed full through the collapse's
	// own drain-and-seed deadline and was force-closed — the WS layer
	// reconnects it). Wired from internal/runtime's WSMetrics seam.
	WSDropped      *prometheus.CounterVec
	SceneRecompute *prometheus.CounterVec
	SceneSwitch    prometheus.Counter
	PushTotal      *prometheus.CounterVec
	PushDuration   *prometheus.HistogramVec
	AdapterErrors  *prometheus.CounterVec

	// Exec-layer observability (ADR 003 §3.1.6, issues #82/#83).
	// EventShed counts B5 back-pressure sheds of NEW fires
	// (`orion_event_shed_total`) — never a killed task. TaskPreempt
	// counts time-slice yields (`orion_task_preempt_total`).
	// ParkedTasks gauges parked continuations (`orion_parked_tasks`);
	// TimerWheelSize (`orion_timer_wheel_size`) gauges armed timer
	// entries. ParkDropped (`orion_exec_park_dropped_total`) counts
	// dropped parks by reason: "duplicate_key" (C1 ordering anomaly)
	// and "cap" (B8 shed of a NEW park). ResumeStale
	// (`orion_exec_resume_stale_total`) counts resumes dropped by the
	// version/epoch wake-key stamp after a cancellation (§3.1.4).
	// TaskCPUSeconds (`orion_task_cpu_seconds_total`) accumulates the
	// per-scene-version aggregate exec CPU (ADR 003 §3.1.6 B7, issue
	// #89): time-slicing protects the scene loop, not the host, so a
	// validated-then-pathological scene is made OBSERVABLE here. It is
	// an incident signal — the alert threshold + runbook and the
	// compose-level CPU isolation belong to Keeper; the engine never
	// kills off it (doctrine §1.1).
	EventShed      *prometheus.CounterVec
	TaskPreempt    *prometheus.CounterVec
	TaskCPUSeconds *prometheus.CounterVec
	ParkedTasks    *prometheus.GaugeVec
	TimerWheelSize *prometheus.GaugeVec
	ParkDropped    *prometheus.CounterVec
	ResumeStale    *prometheus.CounterVec

	// InboxDrop counts writes a scene's event loop refused (full inbox
	// channel) — `orion_inbox_dropped_total` (ADR 003 §3.3 E2, issue
	// #84). The inbox consumes scene.Input's return and increments here
	// instead of silently discarding the loss.
	InboxDrop *prometheus.CounterVec

	// Phase-3 async effects (ADR 003 §3.1.3, issue #85).
	// HTTPEgressBlocked (`orion_http_egress_blocked_total`) counts
	// `http.request` egress-policy denials (R2/B1) — the effect resumes
	// down its `error` port, never a crash. EffectCompletionDropped
	// (`orion_effect_completion_dropped_total`) counts worker-pool
	// completions a persistently full scene inbox refused.
	HTTPEgressBlock *prometheus.CounterVec
	EffectComplDrop *prometheus.CounterVec

	// EgressBudgetExc (`orion_egress_budget_exceeded_total`) counts
	// `service.call` effects denied by the per-stream egress budget (ADR
	// Blue 009 §B / R3) — the node fails closed to its `error` port. A
	// rising count signals a stream hitting (or being abused into) its
	// curated-egress rate cap.
	EgressBudgetExc *prometheus.CounterVec

	// External completion endpoint (B-syswrite, issue #86).
	// CompletionRejected (`orion_exec_completion_rejected_total`)
	// counts dropped animation completion reports by reason: "role"
	// (scope/role gate), "scene" (unknown scene), "kind", "malformed",
	// "inbox" (full scene inbox) at the endpoint; "unknown" (no parked
	// continuation — forged, cross-scene, or the benign loser of the
	// report-vs-fallback race) from the runtime's resume gate. Stale
	// version/epoch drops stay on `orion_exec_resume_stale_total`.
	ComplRejected *prometheus.CounterVec

	// IdempotencyEvict (`orion_idempotency_evicted_total{reason}`) counts
	// scene-intent replay/dedup cache evictions by reason: "ttl" (entry
	// aged past the replay window) or "capacity" (cache at its entry cap,
	// oldest evicted) — ADR-BLUE-012 §12/B8, B3-R6-OPS-ORION. A sustained
	// "capacity" rate signals the window/cap pair is undersized for the
	// deployment's actual scene-intent traffic — the alert condition.
	IdempotencyEvict *prometheus.CounterVec

	// LSDPSnapshotIdentityGap (`orion_lsdp_snapshot_identity_gap_total{scene_id}`)
	// counts an LSDP Snapshot reseed (bootstrap or scene switch, driven by
	// internal/lsdp.sceneMirror — NOT the kit's own per-subscriber
	// backpressure collapse, which is internal to the pinned
	// Lumencast/lumencast-go@v0.3.1 kit and exposes no hook) that dropped a
	// KNOWN projection identity because protocol.Snapshot has no metadata
	// field, on either side of the wire (ADR-BLUE-012 §16.1,
	// B3-R6-16-ORION-PGM). Never incremented when no identity was known yet
	// — that is not a gap.
	LSDPSnapshotIdentityGap *prometheus.CounterVec
}

// CompletionRejected counts an endpoint-level completion drop (#86).
func (m *Metrics) CompletionRejected(sceneID, reason string) {
	m.ComplRejected.WithLabelValues(sceneID, reason).Inc()
}

// ExecResumeUnknown implements the runtime's ExecMetrics seam (#86).
func (m *Metrics) ExecResumeUnknown(sceneID string) {
	m.ComplRejected.WithLabelValues(sceneID, "unknown").Inc()
}

// HTTPEgressBlocked implements the runtime's EffectMetrics seam.
func (m *Metrics) HTTPEgressBlocked(sceneID string) {
	m.HTTPEgressBlock.WithLabelValues(sceneID).Inc()
}

// EffectCompletionDropped implements the runtime's EffectMetrics seam.
func (m *Metrics) EffectCompletionDropped(sceneID string) {
	m.EffectComplDrop.WithLabelValues(sceneID).Inc()
}

// EgressBudgetExceeded implements the runtime's EffectMetrics seam (R3).
func (m *Metrics) EgressBudgetExceeded(sceneID string) {
	m.EgressBudgetExc.WithLabelValues(sceneID).Inc()
}

// InboxDropped implements the adapters' InboxMetrics seam.
func (m *Metrics) InboxDropped(sceneID string) {
	m.InboxDrop.WithLabelValues(sceneID).Inc()
}

// ExecEventShed implements the runtime's ExecMetrics seam.
func (m *Metrics) ExecEventShed(sceneID string) {
	m.EventShed.WithLabelValues(sceneID).Inc()
}

// ExecTaskPreempt implements the runtime's ExecMetrics seam.
func (m *Metrics) ExecTaskPreempt(sceneID string) {
	m.TaskPreempt.WithLabelValues(sceneID).Inc()
}

// ExecCPUSeconds implements the runtime's ExecMetrics seam (B7, #89):
// one exec slice's elapsed seconds, accumulated per (scene_id,
// scene_version).
func (m *Metrics) ExecCPUSeconds(sceneID, sceneVersion string, seconds float64) {
	m.TaskCPUSeconds.WithLabelValues(sceneID, sceneVersion).Add(seconds)
}

// ExecParkedTasks implements the runtime's ExecMetrics seam.
func (m *Metrics) ExecParkedTasks(sceneID string, n int) {
	m.ParkedTasks.WithLabelValues(sceneID).Set(float64(n))
}

// ExecTimerWheelSize implements the runtime's ExecMetrics seam.
func (m *Metrics) ExecTimerWheelSize(sceneID string, n int) {
	m.TimerWheelSize.WithLabelValues(sceneID).Set(float64(n))
}

// ExecParkDropped implements the runtime's ExecMetrics seam.
func (m *Metrics) ExecParkDropped(sceneID, reason string) {
	m.ParkDropped.WithLabelValues(sceneID, reason).Inc()
}

// ExecResumeStale implements the runtime's ExecMetrics seam.
func (m *Metrics) ExecResumeStale(sceneID string) {
	m.ResumeStale.WithLabelValues(sceneID).Inc()
}

// IdempotencyEvicted implements the api package's IdempotencyMetrics seam.
func (m *Metrics) IdempotencyEvicted(reason string) {
	m.IdempotencyEvict.WithLabelValues(reason).Inc()
}

// SnapshotIdentityGap implements the lsdp package's SnapshotMetrics seam.
func (m *Metrics) SnapshotIdentityGap(sceneID string) {
	m.LSDPSnapshotIdentityGap.WithLabelValues(sceneID).Inc()
}

// WSCollapsed implements the runtime's WSMetrics seam (Orion#274).
func (m *Metrics) WSCollapsed(sceneID string) {
	m.WSDropped.WithLabelValues(sceneID, "collapse").Inc()
}

// WSStuckClosed implements the runtime's WSMetrics seam (Orion#274).
func (m *Metrics) WSStuckClosed(sceneID string) {
	m.WSDropped.WithLabelValues(sceneID, "stuck_timeout").Inc()
}

// NewMetrics builds a fresh registry with Orion's metric set.
func NewMetrics() *Metrics {
	r := prometheus.NewRegistry()
	m := &Metrics{
		Registry: r,
		WSConnections: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{Namespace: "orion", Subsystem: "ws", Name: "connections"},
			[]string{"endpoint", "role"},
		),
		WSMessagesIn: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "ws", Name: "messages_in_total"},
			[]string{"type"},
		),
		WSMessagesOut: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "ws", Name: "messages_out_total"},
			[]string{"type"},
		),
		WSDropped: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "ws", Name: "dropped_total"},
			[]string{"scene_id", "reason"},
		),
		SceneRecompute: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "scene", Name: "recompute_total"},
			[]string{"scene_id"},
		),
		SceneSwitch: prometheus.NewCounter(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "scene", Name: "switch_total"},
		),
		PushTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "push", Name: "total"},
			[]string{"outcome"},
		),
		PushDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "orion", Subsystem: "push", Name: "duration_seconds",
				Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.2, 0.5, 1, 2, 5},
			},
			[]string{"outcome"},
		),
		AdapterErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "adapter", Name: "errors_total"},
			[]string{"kind", "key"},
		),
		EventShed: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "event", Name: "shed_total"},
			[]string{"scene_id"},
		),
		TaskPreempt: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "task", Name: "preempt_total"},
			[]string{"scene_id"},
		),
		TaskCPUSeconds: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "task", Name: "cpu_seconds_total"},
			[]string{"scene_id", "scene_version"},
		),
		ParkedTasks: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{Namespace: "orion", Subsystem: "parked", Name: "tasks"},
			[]string{"scene_id"},
		),
		TimerWheelSize: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{Namespace: "orion", Subsystem: "timer", Name: "wheel_size"},
			[]string{"scene_id"},
		),
		ParkDropped: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "exec", Name: "park_dropped_total"},
			[]string{"scene_id", "reason"},
		),
		ResumeStale: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "exec", Name: "resume_stale_total"},
			[]string{"scene_id"},
		),
		InboxDrop: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "inbox", Name: "dropped_total"},
			[]string{"scene_id"},
		),
		HTTPEgressBlock: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "http", Name: "egress_blocked_total"},
			[]string{"scene_id"},
		),
		EffectComplDrop: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "effect", Name: "completion_dropped_total"},
			[]string{"scene_id"},
		),
		EgressBudgetExc: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "egress", Name: "budget_exceeded_total"},
			[]string{"scene_id"},
		),
		ComplRejected: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "exec", Name: "completion_rejected_total"},
			[]string{"scene_id", "reason"},
		),
		IdempotencyEvict: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "idempotency", Name: "evicted_total"},
			[]string{"reason"},
		),
		LSDPSnapshotIdentityGap: prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: "orion", Subsystem: "lsdp", Name: "snapshot_identity_gap_total"},
			[]string{"scene_id"},
		),
	}

	r.MustRegister(
		m.WSConnections,
		m.WSMessagesIn,
		m.WSMessagesOut,
		m.WSDropped,
		m.SceneRecompute,
		m.SceneSwitch,
		m.PushTotal,
		m.PushDuration,
		m.AdapterErrors,
		m.EventShed,
		m.TaskPreempt,
		m.TaskCPUSeconds,
		m.ParkedTasks,
		m.TimerWheelSize,
		m.ParkDropped,
		m.ResumeStale,
		m.InboxDrop,
		m.HTTPEgressBlock,
		m.EffectComplDrop,
		m.EgressBudgetExc,
		m.ComplRejected,
		m.IdempotencyEvict,
		m.LSDPSnapshotIdentityGap,
	)
	return m
}
