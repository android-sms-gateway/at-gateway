package messages

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds the messages module Prometheus counters.
type Metrics struct {
	EnqueuedTotal  prometheus.Counter
	SentTotal      prometheus.Counter
	DeliveredTotal prometheus.Counter
	FailedTotal    prometheus.Counter
	CancelledTotal prometheus.Counter
}

// NewMetrics registers and returns the messages metrics. promauto panics on
// duplicate registration, so tests must build Metrics with plain constructors.
func NewMetrics() *Metrics {
	return &Metrics{
		EnqueuedTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_enqueued_total",
				Help: "Total number of messages enqueued",
			},
		),
		SentTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_sent_total",
				Help: "Total number of messages sent by the worker",
			},
		),
		DeliveredTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_delivered_total",
				Help: "Total number of messages confirmed delivered by a status report",
			},
		),
		FailedTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_failed_total",
				Help: "Total number of messages that failed to send",
			},
		),
		CancelledTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_cancelled_total",
				Help: "Total number of messages cancelled",
			},
		),
	}
}
