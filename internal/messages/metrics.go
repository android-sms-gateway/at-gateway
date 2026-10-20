package messages

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds the messages module Prometheus counters.
type Metrics struct {
	enqueuedTotal  prometheus.Counter
	sentTotal      prometheus.Counter
	deliveredTotal prometheus.Counter
	failedTotal    prometheus.Counter
	cancelledTotal prometheus.Counter
}

// NewMetrics registers and returns the messages metrics. promauto panics on
// duplicate registration, so tests must build Metrics with plain constructors.
func NewMetrics() *Metrics {
	return &Metrics{
		enqueuedTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_enqueued_total",
				Help: "Total number of messages enqueued",
			},
		),
		sentTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_sent_total",
				Help: "Total number of messages sent by the worker",
			},
		),
		deliveredTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_delivered_total",
				Help: "Total number of messages confirmed delivered by a status report",
			},
		),
		failedTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_failed_total",
				Help: "Total number of messages that failed to send",
			},
		),
		cancelledTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_cancelled_total",
				Help: "Total number of messages cancelled",
			},
		),
	}
}

// IncEnqueued increments the enqueued message counter.
func (m *Metrics) IncEnqueued() {
	m.enqueuedTotal.Inc()
}

// IncSent increments the sent message counter.
func (m *Metrics) IncSent() {
	m.sentTotal.Inc()
}

// IncDelivered increments the delivered message counter.
func (m *Metrics) IncDelivered() {
	m.deliveredTotal.Inc()
}

// IncFailed increments the failed message counter.
func (m *Metrics) IncFailed() {
	m.failedTotal.Inc()
}

// IncCancelled increments the cancelled message counter.
func (m *Metrics) IncCancelled() {
	m.cancelledTotal.Inc()
}
