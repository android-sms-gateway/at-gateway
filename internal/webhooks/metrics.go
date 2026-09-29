package webhooks

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds the webhooks module Prometheus counters.
type Metrics struct {
	enqueued          prometheus.Counter
	delivered         prometheus.Counter
	failed            prometheus.Counter
	permanentlyFailed prometheus.Counter
}

// NewMetrics registers and returns the webhooks metrics.
func NewMetrics() *Metrics {
	return newMetrics(prometheus.DefaultRegisterer)
}

func newMetrics(registerer prometheus.Registerer) *Metrics {
	factory := promauto.With(registerer)

	return &Metrics{
		enqueued: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_webhooks_enqueued_total",
				Help: "Total number of webhook deliveries enqueued",
			},
		),
		delivered: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_webhooks_delivered_total",
				Help: "Total number of webhook deliveries completed",
			},
		),
		failed: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_webhooks_failed_total",
				Help: "Total number of webhook delivery attempts that failed",
			},
		),
		permanentlyFailed: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_webhooks_permanently_failed_total",
				Help: "Total number of webhook deliveries that permanently failed",
			},
		),
	}
}

// IncEnqueued increments the enqueued delivery counter.
func (m *Metrics) IncEnqueued() {
	m.enqueued.Inc()
}

// IncDelivered increments the delivered delivery counter.
func (m *Metrics) IncDelivered() {
	m.delivered.Inc()
}

// IncFailed increments the failed delivery attempt counter.
func (m *Metrics) IncFailed() {
	m.failed.Inc()
}

// IncPermanentlyFailed increments the permanently failed delivery counter.
func (m *Metrics) IncPermanentlyFailed() {
	m.permanentlyFailed.Inc()
}
