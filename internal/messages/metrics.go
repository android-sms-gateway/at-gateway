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

	// Inbound-SMS telemetry. The fields stay unexported and are reachable
	// only through the Inc* mutators below.
	inboundEventsTotal             *prometheus.CounterVec
	inboundReassemblyTimeoutsTotal prometheus.Counter
	inboundReassemblyAbortedTotal  prometheus.Counter
	inboundDedupeBlockedTotal      prometheus.Counter
	inboundDedupePostRestartTotal  prometheus.Counter
}

// inboundReassemblyEvictedHelp documents the ONE eviction counter of this
// module. It counts TWO causes - an elapsed reassembly window and the
// concurrent-group capacity - so the Help names both; the metric NAME stays
// wire-compatible and is not renamed.
const inboundReassemblyEvictedHelp = "Total number of incomplete inbound reassembly groups evicted before all parts " +
	"arrived, by cause: the reassembly window elapsed, or the concurrent-group capacity evicted the oldest group"

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
		inboundEventsTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_inbound_events_total",
				Help: "Total number of inbound webhook events emitted for received messages, by event",
			},
			[]string{"event"},
		),
		inboundReassemblyTimeoutsTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_inbound_reassembly_timeouts_total",
				Help: inboundReassemblyEvictedHelp,
			},
		),
		inboundReassemblyAbortedTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_inbound_reassembly_aborted_total",
				Help: "Total number of incomplete inbound reassembly groups discarded because the worker stopped",
			},
		),
		inboundDedupeBlockedTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_dedupe_blocked_total",
				Help: "Total number of inbound emissions suppressed by the in-window dedupe because the webhook " +
					"was already served for that message id",
			},
		),
		inboundDedupePostRestartTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_messages_dedupe_skipped_post_restart_total",
				Help: "Total number of accepted inbound emissions inside the post-start observation window, where " +
					"the in-memory dedupe carries no memory of a previous process and the delivery cycle restarts",
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

// IncInboundEvent counts one emitted inbound webhook event, labelled with the
// event name (sms:received or sms:data-received).
func (m *Metrics) IncInboundEvent(event string) {
	m.inboundEventsTotal.WithLabelValues(event).Inc()
}

// IncReassemblyTimeout counts one incomplete reassembly group evicted before
// all its parts arrived - either because the reassembly window elapsed or
// because the concurrent-group capacity evicted the oldest group. Both causes
// share the counter, and its Help says so.
func (m *Metrics) IncReassemblyTimeout() {
	m.inboundReassemblyTimeoutsTotal.Inc()
}

// IncReassemblyAborted counts one incomplete reassembly group discarded
// because the worker stopped.
func (m *Metrics) IncReassemblyAborted() {
	m.inboundReassemblyAbortedTotal.Inc()
}

// IncDedupeBlocked counts one inbound emission that the in-window dedupe
// suppressed for at least one already-served webhook.
func (m *Metrics) IncDedupeBlocked() {
	m.inboundDedupeBlockedTotal.Inc()
}

// IncDedupeSkippedPostRestart counts one accepted inbound emission inside the
// post-start observation window (see Service.emitInbound).
func (m *Metrics) IncDedupeSkippedPostRestart() {
	m.inboundDedupePostRestartTotal.Inc()
}
