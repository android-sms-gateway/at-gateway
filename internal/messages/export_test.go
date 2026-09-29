package messages

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// NewTestMetrics exposes an isolated metrics constructor to external tests.
func NewTestMetrics(registerer prometheus.Registerer) *Metrics {
	factory := promauto.With(registerer)

	return &Metrics{
		enqueuedTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_enqueued_total",
				Help: "Total number of messages enqueued",
			},
		),
		sentTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_sent_total",
				Help: "Total number of messages sent by the worker",
			},
		),
		deliveredTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_delivered_total",
				Help: "Total number of messages confirmed delivered by a status report",
			},
		),
		failedTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_failed_total",
				Help: "Total number of messages that failed to send",
			},
		),
		cancelledTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_cancelled_total",
				Help: "Total number of messages cancelled",
			},
		),
	}
}
