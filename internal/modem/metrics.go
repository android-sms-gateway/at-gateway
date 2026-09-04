package modem

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type Metrics struct {
	CommandsTotal               *prometheus.CounterVec
	CommandDuration             prometheus.Histogram
	ModemState                  prometheus.Gauge
	SignalQuality               prometheus.Gauge
	ReconnectsTotal             prometheus.Counter
	DeliveryReportsDroppedTotal prometheus.Counter
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
	}

	return m
}
