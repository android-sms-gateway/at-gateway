package inbox

import (
	"context"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// InboundMessage aliases the unexported assembled-message type so the external
// test package can pin the message identity.
type InboundMessage = inboundMessage

// InboundKey aliases the unexported reassembly key for the same reason.
type InboundKey = inboundKey

// NewTestMetrics exposes an isolated metrics constructor to external tests.
func NewTestMetrics(registerer prometheus.Registerer) *Metrics {
	factory := promauto.With(registerer)

	return &Metrics{
		inboundEventsTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_events_total",
				Help: "Test counter",
			},
			[]string{"event"},
		),
		inboundReassemblyTimeoutsTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_reassembly_timeouts_total",
				Help: "Test counter",
			},
		),
		inboundReassemblyAbortedTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_reassembly_aborted_total",
				Help: "Test counter",
			},
		),
		inboundDedupeBlockedTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_dedupe_blocked_total",
				Help: "Test counter",
			},
		),
		inboundDedupePostRestartTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_dedupe_skipped_post_restart_total",
				Help: "Test counter",
			},
		),
		inboxStoredTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_stored_total",
				Help: "Test counter",
			},
		),
		inboxStoreErrorsTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_inbox_store_errors_total",
				Help: "Test counter",
			},
		),
	}
}

// NewInboundKey builds a reassembly-key fixture: the fields are unexported, so
// the external test package cannot spell the literal itself.
func NewInboundKey(sender string, refWidth, ref int) InboundKey {
	return inboundKey{sender: sender, refWidth: refWidth, ref: ref}
}

// NewInboundMessage builds an assembled-message fixture: its fields are
// unexported, so the external test package cannot spell the literal itself.
func NewInboundMessage(
	sender string,
	content string,
	ported bool,
	port uint16,
	receivedAt time.Time,
	timestampValid bool,
	concat *modem.ConcatRef,
) InboundMessage {
	return inboundMessage{
		sender:         sender,
		content:        content,
		ported:         ported,
		port:           port,
		receivedAt:     receivedAt,
		timestampValid: timestampValid,
		concat:         concat,
	}
}

// HandleInboundPartForTest feeds one decoded part through the consumer, with an
// explicit clock so the dedupe window and the reassembly eviction are
// deterministic.
func (s *Service) HandleInboundPartForTest(part modem.InboundPart, now time.Time) {
	s.handleInboundPart(context.Background(), part, now)
}

// SweepInboundForTest runs the reassembly/dedupe eviction sweep.
func (s *Service) SweepInboundForTest(now time.Time) {
	s.sweepInbound(now)
}

// AbortInboundForTest runs the shutdown abort accounting.
func (s *Service) AbortInboundForTest() {
	s.abortInbound()
}

// SetStartedAtForTest moves the process start, i.e. the origin of the
// post-restart observation window.
func (s *Service) SetStartedAtForTest(at time.Time) {
	s.startedAt = at
}

// InboundGroupsForTest exposes the live reassembly groups.
func (s *Service) InboundGroupsForTest() map[InboundKey]int {
	groups := make(map[InboundKey]int, len(s.inboundGroups))
	for key, group := range s.inboundGroups {
		groups[key] = len(group.parts)
	}

	return groups
}

// DedupeForTest exposes the deduper so the external test can pin its capacity,
// TTL and per-webhook marks without reaching into the consumer.
func (s *Service) DedupeForTest() *inboundDeduper {
	return s.inboundDedup
}

// MarkServedForTest records one served webhook for a message id.
func (d *inboundDeduper) MarkServedForTest(messageID, webhookID string, now time.Time) {
	d.mark(messageID, webhookID, now)
}

// ServedForTest reads the already-served webhooks of a message id.
func (d *inboundDeduper) ServedForTest(messageID string, now time.Time) []string {
	return d.served(messageID, now)
}

// SweepForTest runs the dedupe eviction sweep.
func (d *inboundDeduper) SweepForTest(now time.Time) {
	d.sweep(now)
}

// CapacityForTest exposes the pinned dedupe capacity.
func (d *inboundDeduper) CapacityForTest() int {
	return d.capacity
}

// TTLForTest exposes the pinned dedupe TTL, so an external test can step an
// explicit clock past the window without hard-coding the constant.
func (d *inboundDeduper) TTLForTest() time.Duration {
	return d.ttl
}

// LenForTest reports how many message ids the dedupe remembers.
func (d *inboundDeduper) LenForTest() int {
	return len(d.entries)
}

// InboundCounterValue reads a named inbound counter of the inbox metrics for
// external tests. An empty name reads the {event="sms:received"} slice of the
// inbound-event counter.
func InboundCounterValue(m *Metrics, name string) float64 {
	if name == "at_gateway_inbox_events_total" {
		return testutil.ToFloat64(m.inboundEventsTotal.WithLabelValues("sms:received"))
	}

	switch name {
	case "at_gateway_inbox_events_total:data":
		return testutil.ToFloat64(m.inboundEventsTotal.WithLabelValues("sms:data-received"))
	case "at_gateway_inbox_reassembly_timeouts_total":
		return testutil.ToFloat64(m.inboundReassemblyTimeoutsTotal)
	case "at_gateway_inbox_reassembly_aborted_total":
		return testutil.ToFloat64(m.inboundReassemblyAbortedTotal)
	case "at_gateway_inbox_dedupe_blocked_total":
		return testutil.ToFloat64(m.inboundDedupeBlockedTotal)
	case "at_gateway_inbox_dedupe_skipped_post_restart_total":
		return testutil.ToFloat64(m.inboundDedupePostRestartTotal)
	case "at_gateway_inbox_stored_total":
		return testutil.ToFloat64(m.inboxStoredTotal)
	case "at_gateway_inbox_store_errors_total":
		return testutil.ToFloat64(m.inboxStoreErrorsTotal)
	default:
		panic("unknown inbox counter: " + name)
	}
}

// InboundStoreTimeout exposes the bound of ONE store write, so an external
// test can hold the store busy for longer than the bound without hard-coding
// the constant.
func InboundStoreTimeout() time.Duration {
	return inboundStoreTimeout
}

// InboundMessageID exposes the content-deterministic message identity.
func InboundMessageID(m InboundMessage) string {
	return inboundMessageID(m)
}
