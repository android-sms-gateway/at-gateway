//nolint:paralleltest // control and treatment subtests share one global Prometheus registry: control asserts the counter did not move while treatment moves it
package modem_test

import (
	"context"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/inbox"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// inboxAbortedMetric is the ONE observable of the lifecycle proof: an
// incomplete reassembly group discarded because the inbox worker stopped. It
// can only move when inbox.Module(true) registered a runnable that consumed
// the REAL channel and then aborted its incomplete state on Stop.
const inboxAbortedMetric = "at_gateway_inbox_reassembly_aborted_total"

// TestInboxGraphLifecycle is the differential runtime proof behind the static
// single-consumer wave gate: the same modem stub feeds the same part 1 of 2
// through the REAL InboundParts channel into two graphs that differ ONLY in
// inbox.Module(true) vs inbox.Module(false), and the counter movement across
// each graph is read from the Prometheus registry:
//
//   - treatment: the runnable consumes the part (channel drains), the group is
//     still incomplete at Stop, so abortInbound moves the counter by >= 1;
//   - control: no runnable exists, nothing consumes, the counter does not move.
//
// HandleInboundPartForTest is deliberately unused: a direct service feed would
// bypass the channel and prove nothing about the consumer wiring.
func TestInboxGraphLifecycle(t *testing.T) {
	t.Run("runnable consumes the real channel and aborts on stop", func(t *testing.T) {
		if gatheredFamily() {
			// A repeated -count iteration cannot rebuild the graph: inbox
			// NewMetrics registers through promauto, which panics on a second
			// registration. The first iteration runs the full proof below and
			// its value survives in the registry.
			t.Skipf("%s already registered by a previous -count iteration", inboxAbortedMetric)
		}

		before := gatheredCounter(t)
		runInboxGraph(t, true)
		if moved := gatheredCounter(t) - before; moved < 1 {
			t.Errorf(
				"%s moved by %v, want >= 1: the runnable must consume the channel and abort the incomplete group on stop",
				inboxAbortedMetric,
				moved,
			)
		}
	})

	t.Run("without the runnable nothing consumes", func(t *testing.T) {
		before := gatheredCounter(t)
		runInboxGraph(t, false)
		if moved := gatheredCounter(t) - before; moved != 0 {
			t.Errorf(
				"%s moved by %v, want 0: inbox.Module(false) must register no runnable",
				inboxAbortedMetric,
				moved,
			)
		}
	})
}

// runInboxGraph builds one inbox fx graph, starts it, pushes part 1 of 2
// through the modem stub's real channel and stops the graph (which cancels the
// inbox Run context and waits for it, so any abort accounting has landed by
// the time this returns).
func runInboxGraph(t *testing.T, withRun bool) {
	t.Helper()

	webhooksSvc, _ := newWebhookFixture(t)
	modemSvc := modem.NewService(modem.Config{
		Port:           "",
		BaudRate:       0,
		InitTimeout:    0,
		CommandTimeout: 0,
		Cmgd: modem.CmgdConfig{
			RetryAttempts: 0,
			RetryBackoff:  0,
		},
		Store: modem.StoreConfig{
			Memory:        "",
			SweepInterval: 0,
		},
	}, zap.NewNop(), modem.NewTestMetrics(), webhooksSvc)

	app := fx.New(
		fx.NopLogger,
		inbox.Module(withRun),
		fx.Supply(zap.NewNop()),
		fx.Supply(inbox.Config{
			ReassemblyTimeout: 5 * time.Minute,
			SweepInterval:     time.Second,
		}),
		fx.Supply(modemSvc),
		fx.Supply(webhooksSvc),
	)

	startCtx, cancelStart := context.WithTimeout(context.Background(), appStartTimeout)
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start inbox graph: %v", err)
	}

	// PART 1 OF 2: the group stays incomplete, so no webhook is enqueued and
	// the ONLY observable of a consumed-then-stopped run is the abort counter.
	part := modem.InboundPart{
		Sender:         "+79990001234",
		ReceivedAt:     time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC),
		TimestampValid: true,
		Content:        "aaaa",
		Port:           0,
		Ported:         false,
		Concat: &modem.ConcatRef{
			Segments: 2,
			Sequence: 1,
			Ref:      0x07,
			RefWidth: 8,
		},
	}
	if accepted := modemSvc.PushInboundPartForTest(part); !accepted {
		t.Fatal("part 1 of 2 was not accepted into the real InboundParts channel")
	}

	if withRun {
		// Deterministic consumption proof: the run loop has taken the part off
		// the real channel. Only then does the stop below abort a group that
		// provably exists.
		if !waitInboundChannelDrained(modemSvc, appStartTimeout) {
			t.Fatalf("inbox runnable did not consume the pushed part within %s", appStartTimeout)
		}
	} else {
		// No consumer exists, but a (broken) one would show up within this
		// bounded window; either way the control asserts no counter movement.
		waitInboundChannelDrained(modemSvc, 300*time.Millisecond)
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), appStopTimeout)
	defer cancelStop()
	if err := app.Stop(stopCtx); err != nil {
		t.Fatalf("stop inbox graph: %v", err)
	}
}

// waitInboundChannelDrained polls until the real channel is empty or the
// timeout elapses and reports which happened.
func waitInboundChannelDrained(modemSvc *modem.Service, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for len(modemSvc.InboundParts()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	return len(modemSvc.InboundParts()) == 0
}

// gatheredFamily reports whether the abort-counter family is present in the
// default registry.
func gatheredFamily() bool {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return false
	}

	for _, family := range families {
		if family.GetName() == inboxAbortedMetric {
			return true
		}
	}

	return false
}

// gatheredCounter reads the abort counter's value from the default registry;
// an unregistered family reads 0 (the control may not even construct the inbox
// metrics).
func gatheredCounter(t *testing.T) float64 {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	for _, family := range families {
		if family.GetName() != inboxAbortedMetric || len(family.GetMetric()) == 0 {
			continue
		}

		return family.GetMetric()[0].GetCounter().GetValue()
	}

	return 0
}
