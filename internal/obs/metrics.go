package obs

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics groups the runtime counters/gauges Orion publishes on the
// internal scrape endpoint. One field per metric so callers reach for
// a typed accessor instead of magic strings.
type Metrics struct {
	Registry *prometheus.Registry

	WSConnections  *prometheus.GaugeVec
	WSMessagesIn   *prometheus.CounterVec
	WSMessagesOut  *prometheus.CounterVec
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
	EventShed      *prometheus.CounterVec
	TaskPreempt    *prometheus.CounterVec
	ParkedTasks    *prometheus.GaugeVec
	TimerWheelSize *prometheus.GaugeVec
	ParkDropped    *prometheus.CounterVec
	ResumeStale    *prometheus.CounterVec

	// InboxDrop counts writes a scene's event loop refused (full inbox
	// channel) — `orion_inbox_dropped_total` (ADR 003 §3.3 E2, issue
	// #84). The inbox consumes scene.Input's return and increments here
	// instead of silently discarding the loss.
	InboxDrop *prometheus.CounterVec
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
			[]string{"reason"},
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
		m.ParkedTasks,
		m.TimerWheelSize,
		m.ParkDropped,
		m.ResumeStale,
		m.InboxDrop,
	)
	return m
}
