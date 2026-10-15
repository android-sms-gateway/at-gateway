package modem

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Init-failure stage labels of at_gateway_modem_init_failures_total.
//
// Every increment means the SAME thing: connect() gave up before the modem ever
// reached StateReady, so no app:started webhook was emitted and the fx app is
// about to stop. The counter is therefore a "the device never came up" signal,
// not a degradation report - a modem that queries a single field badly still
// reaches ready state and MUST NOT be counted here.
const (
	// initStagePortOpen is the serial port that could not be opened.
	initStagePortOpen = "port_open"
)

// initFailureStage turns an init-row tag into the <stage> label of
// at_gateway_modem_init_failures_total: lowercase with spaces as underscores
// ("PDU mode" -> "pdu_mode", "SIM PIN" -> "sim_pin"). Deriving the label from
// the tag keeps the counter vocabulary in ONE place, so a new init row cannot
// drift away from the label its failure is reported under.
func initFailureStage(tag string) string {
	return strings.ToLower(strings.ReplaceAll(tag, " ", "_"))
}

type Metrics struct {
	CommandsTotal               *prometheus.CounterVec
	CommandDuration             prometheus.Histogram
	ModemState                  prometheus.Gauge
	SignalQuality               prometheus.Gauge
	ReconnectsTotal             prometheus.Counter
	DeliveryReportsDroppedTotal prometheus.Counter

	// Inbound-SMS telemetry. The fields stay unexported and are reachable
	// only through the Inc*/Dec* mutators below.
	inboundPartsDropped     *prometheus.CounterVec
	storeCommandFailures    *prometheus.CounterVec
	inboundHandlersInflight prometheus.Gauge
	storeSweeps             prometheus.Counter
	initFailures            *prometheus.CounterVec
}

// NewMetrics registers the modem metrics with the default Prometheus registry
// and returns their handles.
func NewMetrics() *Metrics {
	m := &Metrics{
		CommandsTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "at_gateway_modem_commands_total",
				Help: "Total number of AT commands sent",
			},
			[]string{"command", "status"},
		),
		CommandDuration: promauto.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "at_gateway_modem_command_duration_seconds",
				Help:    "Duration of AT commands in seconds",
				Buckets: []float64{0.1, 0.5, 1, 2, 5},
			},
		),
		ModemState: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "at_gateway_modem_state",
				Help: "Current modem state (0=disconnected, 1=connecting, 2=ready, 3=error)",
			},
		),
		SignalQuality: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "at_gateway_modem_signal_quality_percent",
				Help: "Current signal quality percentage",
			},
		),
		ReconnectsTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_modem_reconnects_total",
				Help: "Total number of modem reconnections",
			},
		),
		DeliveryReportsDroppedTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_modem_delivery_reports_dropped_total",
				Help: "Total number of delivery reports dropped because the consumer channel was full",
			},
		),
		inboundPartsDropped: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "at_gateway_modem_inbound_parts_dropped_total",
				Help: "Total number of inbound SMS parts dropped before emission, by reason " +
					"(handler_saturated, channel_full, malformed_head, not_connected, read_failed, decode_failed, " +
					"channel_full_exhausted, handler_panic); channel_full leaves the modem store entry readable for " +
					"a re-notification, channel_full_exhausted is the final loss: the entry spent its retry budget " +
					"(3 hand-off attempts) or was evicted from the bounded tracking map, and was deleted",
			},
			[]string{"reason"},
		),
		storeCommandFailures: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "at_gateway_modem_store_command_failures_total",
				Help: "Total number of modem message-store commands that failed after all retries, by operation",
			},
			[]string{"op"},
		),
		inboundHandlersInflight: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "at_gateway_modem_inbound_handlers_inflight",
				Help: "Number of +CMTI indication handlers currently running",
			},
		),
		storeSweeps: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_modem_store_sweeps_total",
				Help: "Total number of retained-entry sweeps that listed a non-empty preferred store " +
					"(AT+CMGL=4) to recover entries that were stored but never re-notified",
			},
		),
		initFailures: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "at_gateway_modem_init_failures_total",
				Help: "Total number of times connect() gave up before the modem reached ready state, by stage " +
					"(port_open, or the failing init row's tag in snake_case: test, echo_off, verbose_errors, " +
					"pdu_mode, sms_routing, sim_pin); every increment means no app:started webhook was emitted " +
					"and the process stops",
			},
			[]string{"stage"},
		),
	}

	return m
}

// IncInboundPartsDropped counts one inbound SMS part that never reached the
// consumer, labelled by the drop reason.
func (m *Metrics) IncInboundPartsDropped(reason string) {
	m.inboundPartsDropped.WithLabelValues(reason).Inc()
}

// IncStoreCommandFailure counts one modem message-store command that failed
// after its retries were exhausted, labelled by the operation.
func (m *Metrics) IncStoreCommandFailure(op string) {
	m.storeCommandFailures.WithLabelValues(op).Inc()
}

// IncInboundHandlersInflight records that one more +CMTI handler started.
func (m *Metrics) IncInboundHandlersInflight() {
	m.inboundHandlersInflight.Inc()
}

// DecInboundHandlersInflight records that one +CMTI handler finished.
func (m *Metrics) DecInboundHandlersInflight() {
	m.inboundHandlersInflight.Dec()
}

// IncStoreSweep counts one retained-entry sweep that found at least one store
// entry to re-process.
func (m *Metrics) IncStoreSweep() {
	m.storeSweeps.Inc()
}

// IncInitFailure counts one connect() failure that stopped the modem before it
// ever reached ready state, labelled by the failing stage.
func (m *Metrics) IncInitFailure(stage string) {
	m.initFailures.WithLabelValues(stage).Inc()
}
