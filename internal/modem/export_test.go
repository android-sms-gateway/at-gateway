package modem

import (
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem/port"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// NewTestMetrics builds a fully populated Metrics with PLAIN constructors
// (promauto panics on duplicate registration) for external tests. Every
// counter carries the Help string promlinter demands.
func NewTestMetrics() *Metrics {
	return &Metrics{
		CommandsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "test_commands_total",
				Help: "Test counter",
			},
			[]string{"command", "status"},
		),
		CommandDuration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "test_command_duration_seconds",
				Help:    "Test histogram",
				Buckets: []float64{0.1, 0.5, 1, 2, 5},
			},
		),
		ModemState: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "test_modem_state",
				Help: "Test gauge",
			},
		),
		SignalQuality: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "test_signal_quality_percent",
				Help: "Test gauge",
			},
		),
		ReconnectsTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "test_reconnects_total",
				Help: "Test counter",
			},
		),
		DeliveryReportsDroppedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "test_delivery_reports_dropped_total",
				Help: "Test counter",
			},
		),
		inboundPartsDropped: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "test_inbound_parts_dropped_total",
				Help: "Test counter",
			},
			[]string{"reason"},
		),
		storeCommandFailures: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "test_store_command_failures_total",
				Help: "Test counter",
			},
			[]string{"op"},
		),
		inboundHandlersInflight: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "test_inbound_handlers_inflight",
				Help: "Test gauge",
			},
		),
		storeSweeps: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "test_store_sweeps_total",
				Help: "Test counter",
			},
		),
		initFailures: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "test_init_failures_total",
				Help: "Test counter",
			},
			[]string{"stage"},
		),
	}
}

// InitFailure reads the init-failure counter for one stage.
func InitFailure(m *Metrics, stage string) float64 {
	return testutil.ToFloat64(m.initFailures.WithLabelValues(stage))
}

// InboundPartDropped reads the inbound drop counter for one reason.
func InboundPartDropped(m *Metrics, reason string) float64 {
	return testutil.ToFloat64(m.inboundPartsDropped.WithLabelValues(reason))
}

// StoreCommandFailure reads the store-command failure counter for one op.
func StoreCommandFailure(m *Metrics, op string) float64 {
	return testutil.ToFloat64(m.storeCommandFailures.WithLabelValues(op))
}

// InboundHandlersInflight reads the +CMTI handler occupancy gauge.
func InboundHandlersInflight(m *Metrics) float64 {
	return testutil.ToFloat64(m.inboundHandlersInflight)
}

// StoreSweeps reads the retained-entry sweep counter.
func StoreSweeps(m *Metrics) float64 {
	return testutil.ToFloat64(m.storeSweeps)
}

// SetPortFactory replaces the serial-port factory so external tests can drive
// a scripted modem through the real connect()/indication path (the field is
// the same seam the send tests already rely on).
func (s *Service) SetPortFactory(factory func(port.Config) (port.Port, error)) {
	s.portFactory = factory
}

// ParseStoredReference exposes the +CMTI head parser to external tests.
func ParseStoredReference(head string) (string, int, error) {
	return parseStoredReference(head)
}

// PushInboundPartForTest offers a decoded part to the inbound channel the way
// the +CMTI handler does (non-blocking) and reports whether it was accepted.
func (s *Service) PushInboundPartForTest(part InboundPart) bool {
	select {
	case s.inboundParts <- part:
		return true
	default:
		return false
	}
}

// HandlerSlots exposes the pinned +CMTI handler semaphore capacity.
func (s *Service) HandlerSlots() int {
	return cap(s.inboundSlots)
}

// HandleCMTIForTest invokes the +CMTI indication handler directly so the
// no-Commands and empty-head branches can be exercised without a modem.
func (s *Service) HandleCMTIForTest(info []string) {
	s.handleCMTI(info)
}

// RecoverInboundForTest is the SAME panic guard shape the +CMTI and +CDS
// handlers defer (recover() must sit in the deferred function itself), so the
// external test can pin that a panic is swallowed and counted.
func (s *Service) RecoverInboundForTest() {
	if recovered := recover(); recovered != nil {
		s.reportInboundPanic("+CMTI", recovered)
	}
}

// ChannelFullRetries is the pinned per-store-entry hand-off budget.
func ChannelFullRetries() int {
	return inboundChannelFullRetries
}

// ChannelFullBudgetCap is the pinned cap on tracked full-channel budgets.
func ChannelFullBudgetCap() int {
	return inboundChannelFullRefs
}

// ChannelFullBudgetWindow is how long a booked budget survives without a new
// failure, i.e. when the modem reusing a store slot starts from zero.
func ChannelFullBudgetWindow() time.Duration {
	return inboundChannelFullBudgetWindow
}

// SpendChannelFullForTest books one full-channel hand-off failure for a store
// entry, exactly as the +CMTI handler does, and reports whether a retry is
// still allowed. The observation time is a parameter (as it is in production)
// so a test can cross the budget window without sleeping.
func (s *Service) SpendChannelFullForTest(memory, index int, now time.Time) bool {
	return s.spendChannelFull(storedRef{memory: memory, index: index}, now)
}

// ChannelFullBudgetsForTest counts the tracked full-channel budgets.
func (s *Service) ChannelFullBudgetsForTest() int {
	s.channelFullMu.Lock()
	defer s.channelFullMu.Unlock()

	return len(s.channelFullDrops)
}

// ChannelFullBudgetForTest reads the budget booked for one store entry.
func (s *Service) ChannelFullBudgetForTest(memory, index int) (int, time.Time, bool) {
	s.channelFullMu.Lock()
	defer s.channelFullMu.Unlock()

	budget, tracked := s.channelFullDrops[storedRef{memory: memory, index: index}]

	return budget.attempts, budget.seenAt, tracked
}

// SetSignalRefreshIntervalForTest shortens the Run ticker period so a test can
// drive the lifecycle work that tick performs; the field is unexported and has
// no config key by design.
func (s *Service) SetSignalRefreshIntervalForTest(interval time.Duration) {
	s.signalRefreshInterval = interval
}

// EmitAppStartedForTest re-runs the once-per-process app:started emission the
// way a second StateReady transition would, so a test can prove the guard
// swallows it. It is the same call connect() makes after setState(StateReady).
func (s *Service) EmitAppStartedForTest() {
	s.emitAppStartedOnce()
}
