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
	)
	return m
}
