package modem_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/at-gateway/internal/modem/port"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/bunfx"
	"github.com/go-core-fx/goosefx"
	"github.com/go-core-fx/sqlfx"
	"github.com/uptrace/bun"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// neverReadyMessage is the operator-visible log entry a modem that never
// reaches ready state MUST produce, pinned verbatim.
const neverReadyMessage = "modem never reached ready state; the app:started webhook is not emitted"

// Init-failure stages, mirrored from the production labels so a rename breaks
// the test instead of silently detaching the counter. The assertions sweep the
// whole vocabulary: a failing stage must be counted and no other one.
const (
	initStagePortOpen = "port_open"
	initStagePDUMode  = "pdu_mode"
	initStageSIMPIN   = "sim_pin"
	initStageAT       = "test"
)

// allInitStages is the stage vocabulary an assertion sweeps.
func allInitStages() []string {
	return []string{initStagePortOpen, initStagePDUMode, initStageSIMPIN, initStageAT}
}

// Fixture limits of the in-memory persistence graph every app:started test
// builds (one SQLite connection keeps the in-memory database stable).
const (
	appStartTimeout = 10 * time.Second
	appStopTimeout  = 5 * time.Second
	appSingleConn   = 1
)

// appStartedModem scripts the boot sequence up to ready state: every init row,
// the three identity queries and the five SIM queries, so connect() reaches
// StateReady with a POPULATED SimInfo. pinLocked and rejectedRow turn the
// scripted modem into a failing one - a SIM that is not READY and an init row
// the modem rejects - which is what stops the gateway before it is ever ready.
type appStartedModem struct {
	sims        func(string) [][]byte
	pinLocked   bool
	rejectedRow string
}

// newReadyModem answers the SIM queries with the canned lines the modem SIM
// snapshot is derived from; empty strings model a SIM that reports nothing.
func newReadyModem(cnum, ccid, cops string) *appStartedModem {
	return &appStartedModem{
		sims:        simResponder(cnum, ccid, cops, "+CSQ: 22,99", "+CREG: 0,1"),
		pinLocked:   false,
		rejectedRow: "",
	}
}

func (m *appStartedModem) respond(w string) [][]byte {
	switch {
	case m.pinLocked && w == "AT+CPIN?\r\n":
		return [][]byte{okResponse("+CPIN: SIM PIN2"), okResponse("OK")}
	case m.rejectedRow != "" && w == m.rejectedRow:
		return [][]byte{okResponse("ERROR")}
	}

	if resps := m.sims(w); resps != nil {
		return resps
	}

	switch w {
	case "AT+GMI\r\n":
		return [][]byte{okResponse("MegaFon"), okResponse("OK")}
	case "AT+GMM\r\n":
		return [][]byte{okResponse("MS2131"), okResponse("OK")}
	case "AT+GSN\r\n":
		return [][]byte{okResponse("352093001111111"), okResponse("OK")}
	}

	return nil
}

// newWebhookFixture builds the REAL webhook registry and queue on an in-memory
// SQLite database, so a modem-level test observes the app:started emission as
// the stored queue row the delivery worker would POST and sign.
//
// The metrics argument is nil on purpose: emit() tolerates it and the delivery
// worker never runs here, while webhooks.NewMetrics() registers through
// promauto and would panic on its second call inside this test binary.
func newWebhookFixture(t *testing.T) (*webhooks.Service, *webhooks.Repository) {
	t.Helper()

	var bunDB *bun.DB
	app := fx.New(
		fx.NopLogger,
		fx.Supply(zap.NewNop()),
		fx.Supply(sqlfx.Config{
			URL:             "sqlite://:memory:",
			ConnMaxIdleTime: 0,
			ConnMaxLifetime: 0,
			MaxOpenConns:    appSingleConn,
			MaxIdleConns:    appSingleConn,
		}),
		db.Module(),
		sqlfx.Module(),
		goosefx.Module(),
		bunfx.Module(),
		fx.Invoke(func(b *bun.DB) {
			bunDB = b
		}),
	)

	startCtx, cancelStart := context.WithTimeout(context.Background(), appStartTimeout)
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start persistence app: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancelStop := context.WithTimeout(context.Background(), appStopTimeout)
		defer cancelStop()
		if stopErr := app.Stop(stopCtx); stopErr != nil {
			t.Errorf("stop persistence app: %v", stopErr)
		}
	})

	repo := webhooks.NewRepository(bunDB)
	storageSvc, err := storage.NewService(
		storage.Config{Path: filepath.Join(t.TempDir(), "storage.json")},
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create storage service: %v", err)
	}
	devicesSvc := devices.NewService(devices.Config{Name: "test-device"}, storageSvc, zap.NewNop())
	service, err := webhooks.NewService(
		webhooks.Config{SigningKey: "modem-app-started-test-key"},
		repo,
		devicesSvc,
		storageSvc,
		nil,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create webhooks service: %v", err)
	}

	return service, repo
}

// newAppStartedService wires the modem service on a scripted port with the
// observed ERROR-level log core, so a test can assert both the metrics and the
// operator-visible entries of the failure path.
func newAppStartedService(
	t *testing.T,
	scripted *appStartedModem,
	webhooksSvc *webhooks.Service,
) (*modem.Service, *modem.Metrics, *observer.ObservedLogs) {
	t.Helper()

	core, logs := observer.New(zap.ErrorLevel)
	metrics := modem.NewTestMetrics()
	service := modem.NewService(inboundServiceConfig(), zap.New(core), metrics, webhooksSvc)
	service.SetPortFactory(func(port.Config) (port.Port, error) {
		return &servicePort{&scriptedModem{
			responses: make(chan []byte, 1024),
			onWrite:   scripted.respond,
		}}, nil
	})

	return service, metrics, logs
}

// modemRun drives service.Run in the background. stop is idempotent, so a test
// that needs Run's error (the init-failure path) and the cleanup that cancels a
// healthy Run never race for the same result.
type modemRun struct {
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
	err    error
}

// stop cancels the run context and returns the error Run finished with.
func (r *modemRun) stop(t *testing.T) error {
	t.Helper()

	r.cancel()

	return r.wait(t)
}

// wait blocks until Run returns on its OWN and reports its error. A run that
// never comes up fails inside connect(), so waiting without canceling first is
// what keeps the test from racing its own cancellation against the scripted
// failure.
func (r *modemRun) wait(t *testing.T) error {
	t.Helper()

	r.once.Do(func() {
		select {
		case r.err = <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("modem Run did not return")
		}
	})

	return r.err
}

// runModem starts service.Run in the background and cancels it (awaiting the
// result) at cleanup, so a test never leaks the goroutine.
func runModem(t *testing.T, service *modem.Service) *modemRun {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	run := &modemRun{
		cancel: cancel,
		done:   make(chan error, 1),
		once:   sync.Once{},
		err:    nil,
	}
	go func() {
		run.done <- service.Run(ctx)
	}()
	t.Cleanup(func() {
		run.stop(t)
	})

	return run
}

// queuedEnvelopes reads every stored webhook queue item, i.e. every delivery
// the emission enqueued.
func queuedEnvelopes(t *testing.T, repo *webhooks.Repository) []webhooks.QueueItem {
	t.Helper()

	items, err := repo.DueItems(context.Background(), time.Now().Add(time.Hour), 32)
	if err != nil {
		t.Fatalf("read queued items: %v", err)
	}

	return items
}

// storedPayload returns the RAW stored envelope bytes of the single queued
// item of the given webhook; they are the bytes the worker signs and POSTs.
func storedPayload(t *testing.T, repo *webhooks.Repository, webhookID string) string {
	t.Helper()

	for _, item := range queuedEnvelopes(t, repo) {
		if item.WebhookID == webhookID {
			return item.Payload
		}
	}

	t.Fatalf("no queued envelope for webhook %q", webhookID)

	return ""
}

// envelopePayload returns the payload sub-document of a stored envelope.
func envelopePayload(t *testing.T, raw string) string {
	t.Helper()

	var envelope struct {
		Event   smsgateway.WebhookEvent `json:"event"`
		Payload json.RawMessage         `json:"payload"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode stored envelope %s: %v", raw, err)
	}
	if envelope.Event != smsgateway.WebhookEventAppStarted {
		t.Errorf("envelope event = %q, want app:started", envelope.Event)
	}

	return string(envelope.Payload)
}

// registerWebhooks registers webhooks for the given events, all for ALL
// devices (a nil deviceId), so the local device scope never interferes.
func registerWebhooks(t *testing.T, service *webhooks.Service, hookIDs []string, event smsgateway.WebhookEvent) {
	t.Helper()

	for _, id := range hookIDs {
		if err := service.Replace(context.Background(), &smsgateway.Webhook{
			ID:       id,
			DeviceID: nil,
			URL:      "https://example.com/" + id,
			Event:    event,
		}); err != nil {
			t.Fatalf("replace webhook %s: %v", id, err)
		}
	}
}

// TestAppStarted_EmittedOncePerProcessAtReady pins the whole emission: the
// FIRST StateReady transition enqueues exactly ONE envelope per registered
// app:started webhook (never for another event), the payload carries the SIM
// snapshot read at ready time, and a second ready transition never re-emits.
func TestAppStarted_EmittedOncePerProcessAtReady(t *testing.T) {
	const (
		hookFirst  = "app-started-first"
		hookOther  = "app-started-other-event"
		hookSecond = "app-started-second"
	)

	webhooksSvc, repo := newWebhookFixture(t)
	registerWebhooks(t, webhooksSvc, []string{hookFirst, hookSecond}, smsgateway.WebhookEventAppStarted)
	registerWebhooks(t, webhooksSvc, []string{hookOther}, smsgateway.WebhookEventSmsSent)

	service, metrics, _ := newAppStartedService(t, newReadyModem(
		`+CNUM: "MegaFon","+79991234567",145`,
		"+CCID: 25001123456789012345",
		`+COPS: 0,0,"MegaFon",7`,
	), webhooksSvc)
	runModem(t, service)

	waitForCount(t, "one app:started envelope per registered webhook", func() bool {
		return len(queuedEnvelopes(t, repo)) == 2
	})

	// The SIM attributes are pinned AS THE MODEM SNAPSHOT HOLDS THEM, with the
	// ICCID NORMALIZED: GetSimInfo strips the "+CCID: " response prefix, so the
	// published identifier is the bare ICCID the consumer expects.
	want := `{"simCards":[{"slotIndex":0,"simNumber":1,"phoneNumber":"+79991234567",` +
		`"carrierName":"MegaFon","iccid":"25001123456789012345"}]}`
	for _, hookID := range []string{hookFirst, hookSecond} {
		if got := envelopePayload(t, storedPayload(t, repo, hookID)); got != want {
			t.Errorf("payload for %s = %s, want %s", hookID, got, want)
		}
	}

	for _, stage := range allInitStages() {
		if got := modem.InitFailure(metrics, stage); got != 0 {
			t.Errorf("init failures stage=%s = %v, want 0 on a ready modem", stage, got)
		}
	}

	// A SECOND StateReady transition must not emit again: connect() runs once
	// per process, so the once-guard is what keeps the event singular even if a
	// future reconnect loop is added.
	service.EmitAppStartedForTest()
	service.EmitAppStartedForTest()

	if got := len(queuedEnvelopes(t, repo)); got != 2 {
		t.Errorf("queued items after a second ready transition = %d, want the original 2", got)
	}
}

// TestAppStarted_UnreportedSIMFieldsAreOmitted pins the modem-side mapping of a
// SIM that reports nothing optional: the mandatory slot index and SIM number are
// always present, every unreported attribute is OMITTED from the wire payload
// (client-go omitempty pointers) and never sent as an empty string.
func TestAppStarted_UnreportedSIMFieldsAreOmitted(t *testing.T) {
	const hookID = "app-started-empty-sim"

	webhooksSvc, repo := newWebhookFixture(t)
	registerWebhooks(t, webhooksSvc, []string{hookID}, smsgateway.WebhookEventAppStarted)

	service, _, _ := newAppStartedService(t, newReadyModem("", "", ""), webhooksSvc)
	runModem(t, service)

	waitForCount(t, "the app:started envelope", func() bool {
		return len(queuedEnvelopes(t, repo)) == 1
	})

	if got, want := envelopePayload(t, storedPayload(t, repo, hookID)),
		`{"simCards":[{"slotIndex":0,"simNumber":1}]}`; got != want {
		t.Errorf("payload = %s, want %s", got, want)
	}
}

// TestAppStarted_NoRegisteredWebhookStoresNothing pins the unregistered
// boundary end to end: an emission nobody registered for is a no-op that stores
// no queue row, so a device without an app:started webhook pays no DB work.
func TestAppStarted_NoRegisteredWebhookStoresNothing(t *testing.T) {
	webhooksSvc, repo := newWebhookFixture(t)
	registerWebhooks(t, webhooksSvc, []string{"sms-sent-only"}, smsgateway.WebhookEventSmsSent)

	service, _, _ := newAppStartedService(t, newReadyModem(
		`+CNUM: "MegaFon","+79991234567",145`,
		"+CCID: 25001123456789012345",
		`+COPS: 0,0,"MegaFon",7`,
	), webhooksSvc)
	runModem(t, service)

	waitForCount(t, "the modem to become ready", func() bool {
		return service.State() == modem.StateReady
	})

	if got := len(queuedEnvelopes(t, repo)); got != 0 {
		t.Errorf("queued items = %d, want 0 without a registered webhook", got)
	}
}

// TestAppStarted_NotEmittedBeforeReadyState pins the emission TIMING: the event
// belongs to the first StateReady transition, so a modem that never gets there
// (here: one that answers NOTHING, so even the first init row times out) leaves
// NO envelope behind - neither while it is still initializing nor after it gave
// up. An emission bound to fx OnStart, which planning explicitly rejected,
// would store a row inside the loop below.
func TestAppStarted_NotEmittedBeforeReadyState(t *testing.T) {
	const hookID = "app-started-before-ready"

	webhooksSvc, repo := newWebhookFixture(t)
	registerWebhooks(t, webhooksSvc, []string{hookID}, smsgateway.WebhookEventAppStarted)

	// A modem that never answers: every init row hits the command timeout, so
	// the service stays in the connecting state and connect() gives up.
	silent := &appStartedModem{sims: func(string) [][]byte { return nil }}
	service, metrics, _ := newAppStartedService(t, silent, webhooksSvc)
	run := runModem(t, service)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state := service.State()
		if state == modem.StateReady {
			t.Fatal("modem state = ready, want the scripted timeout to keep it from ever getting there")
		}
		if got := len(queuedEnvelopes(t, repo)); got != 0 {
			t.Fatalf("queued items = %d while the modem is %s, want 0", got, state)
		}
		if state == modem.StateError {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	if err := run.wait(t); err == nil {
		t.Fatal("Run error = nil, want the init timeout that stops the app")
	}
	if got := len(queuedEnvelopes(t, repo)); got != 0 {
		t.Errorf("queued items = %d after the init failure, want 0", got)
	}
	if got := modem.InitFailure(metrics, initStageAT); got != 1 {
		t.Errorf("init failures stage=%s = %v, want 1", initStageAT, got)
	}
}

// TestInitFailure_NeverReachesReadyNorEmits pins round-2 fix 8 on every
// connect() failure path: the modem stops in the error state, the init-failure
// counter is incremented EXACTLY ONCE with the failing stage (a rejected init
// row is labelled by that row's tag), an explicit error-level "never reached
// ready" entry is logged before the error that stops the app, and NO app:started
// envelope is stored.
func TestInitFailure_NeverReachesReadyNorEmits(t *testing.T) {
	tests := []struct {
		name  string
		stage string
		setup func(*appStartedModem)
		open  func(*testing.T, *modem.Service)
	}{
		{
			name:  "the serial port cannot be opened",
			stage: initStagePortOpen,
			setup: func(*appStartedModem) {},
			open: func(t *testing.T, service *modem.Service) {
				t.Helper()
				service.SetPortFactory(func(port.Config) (port.Port, error) {
					return nil, errors.New("open /dev/null: no such device")
				})
			},
		},
		{
			name:  "an init row is rejected",
			stage: initStagePDUMode,
			setup: func(m *appStartedModem) {
				m.rejectedRow = "AT+CMGF=0\r\n"
			},
			open: func(*testing.T, *modem.Service) {},
		},
		{
			name:  "the SIM PIN is not ready",
			stage: initStageSIMPIN,
			setup: func(m *appStartedModem) {
				m.pinLocked = true
			},
			open: func(*testing.T, *modem.Service) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const hookID = "app-started-after-init-failure"

			webhooksSvc, repo := newWebhookFixture(t)
			registerWebhooks(t, webhooksSvc, []string{hookID}, smsgateway.WebhookEventAppStarted)

			scripted := newReadyModem("", "", "")
			tt.setup(scripted)

			service, metrics, logs := newAppStartedService(t, scripted, webhooksSvc)
			tt.open(t, service)
			run := runModem(t, service)

			if err := run.wait(t); err == nil {
				t.Fatal("Run error = nil, want the connect failure that stops the app")
			}

			if state := service.State(); state != modem.StateError {
				t.Errorf("modem state = %s, want error", state)
			}

			if got := modem.InitFailure(metrics, tt.stage); got != 1 {
				t.Errorf("init failures stage=%s = %v, want 1", tt.stage, got)
			}
			for _, stage := range allInitStages() {
				if stage == tt.stage {
					continue
				}
				if got := modem.InitFailure(metrics, stage); got != 0 {
					t.Errorf("init failures stage=%s = %v, want 0 for a %s failure", stage, got, tt.stage)
				}
			}

			if entries := logs.FilterMessage(neverReadyMessage).Len(); entries != 1 {
				t.Errorf("error logs %q = %d, want exactly one before the app stops", neverReadyMessage, entries)
			}
			if logs.Len() == 0 {
				t.Error("no error-level log entries at all")
			}

			if got := len(queuedEnvelopes(t, repo)); got != 0 {
				t.Errorf("queued items = %d, want 0 - app:started must not survive a failed init", got)
			}
		})
	}
}
