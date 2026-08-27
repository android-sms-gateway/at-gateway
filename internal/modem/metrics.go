package modem

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type Metrics struct {
	CommandsTotal    *prometheus.CounterVec
	CommandDuration  prometheus.Histogram
	SMSSentTotal     prometheus.Counter
	SMSReceivedTotal prometheus.Counter
	ModemState       prometheus.Gauge
	SignalQuality    prometheus.Gauge
	ReconnectsTotal  prometheus.Counter
}

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
		SMSSentTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_modem_sms_sent_total",
				Help: "Total number of SMS sent",
			},
		),
		SMSReceivedTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "at_gateway_modem_sms_received_total",
				Help: "Total number of SMS received",
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
	}

	return m
}
