package inbox

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// inboundReassemblyEvictedHelp documents the ONE eviction counter of this
// module. It counts TWO causes - an elapsed reassembly window and the
// concurrent-group capacity - so the Help names both.
const inboundReassemblyEvictedHelp = "Total number of incomplete inbound reassembly groups evicted before all parts " +
	"arrived, by cause: the reassembly window elapsed, or the concurrent-group capacity evicted the oldest group"

// Metrics holds the inbox module Prometheus counters. The fields stay
// unexported and are reachable only through the Inc* mutators below.
type Metrics struct {
	inboundEventsTotal             *prometheus.CounterVec
	inboundReassemblyTimeoutsTotal prometheus.Counter
	inboundReassemblyAbortedTotal  prometheus.Counter
	inboundDedupeBlockedTotal      prometheus.Counter
	inboundDedupePostRestartTotal  prometheus.Counter
	inboxStoredTotal               prometheus.Counter
	inboxStoreConflictsTotal       prometheus.Counter
	inboxStoreErrorsTotal          prometheus.Counter
}

// NewMetrics registers and returns the inbox metrics. promauto panics on
// duplicate registration, so tests must build Metrics with plain constructors.
func NewMetrics() *Metrics {
	return &Metrics{
		inboundEventsTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_events_total",
				Help: "Total number of inbound webhook events emitted for received messages, by event",
			},
			[]string{"event"},
		),
		inboundReassemblyTimeoutsTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_reassembly_timeouts_total",
				Help: inboundReassemblyEvictedHelp,
			},
		),
		inboundReassemblyAbortedTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_reassembly_aborted_total",
				Help: "Total number of incomplete inbound reassembly groups discarded because the worker stopped",
			},
		),
		inboundDedupeBlockedTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_dedupe_blocked_total",
				Help: "Total number of inbound emissions suppressed by the in-window dedupe because the webhook " +
					"was already served for that message id",
			},
		),
		inboundDedupePostRestartTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_dedupe_skipped_post_restart_total",
				Help: "Total number of accepted inbound emissions inside the post-start observation window, where " +
					"the in-memory dedupe carries no memory of a previous process and the delivery cycle restarts",
			},
		),
		inboxStoredTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_stored_total",
				Help: "Total number of inbound messages persisted in the local inbox store",
			},
		),
		inboxStoreConflictsTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_store_conflicts_total",
				Help: "Total number of inbound inbox insert attempts that hit the (device_id, message_id) " +
					"unique conflict and were ignored because the row already existed: the accepted D-1 " +
					"collapse of byte-identical content, or a modem re-notification of a message already stored",
			},
		),
		inboxStoreErrorsTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_store_errors_total",
				Help: "Total number of inbound messages that could not be persisted in the local inbox store, " +
					"e.g. because the store write failed or timed out",
			},
		),
	}
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

// IncStored counts one inbound message persisted in the local inbox store.
func (m *Metrics) IncStored() {
	m.inboxStoredTotal.Inc()
}

// IncStoreConflict counts one inbox insert attempt that hit the
// (device_id, message_id) unique conflict and was ignored because the row
// already existed - the accepted D-1 collapse of byte-identical content, or a
// modem re-notification of a message already stored. It is NOT a store
// failure: the row is present, only the write was skipped.
func (m *Metrics) IncStoreConflict() {
	m.inboxStoreConflictsTotal.Inc()
}

// IncStoreFailed counts one inbound message that could NOT be persisted in the
// local inbox store, e.g. because the store write failed or timed out. The
// webhook emission of such a message still happens: the row is the durable
// record, the emission is the notification.
func (m *Metrics) IncStoreFailed() {
	m.inboxStoreErrorsTotal.Inc()
}
