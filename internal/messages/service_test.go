//nolint:testpackage // in-package tests build Service with the concrete fakeSender stub and assert DTO mapping without exported seams.
package messages

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db/migrations"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/bunfx"
	"github.com/go-core-fx/goosefx"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"go.uber.org/zap"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

const (
	testMaxConns          = 1
	testPollInterval      = 5 * time.Millisecond
	testWaitTimeout       = 5 * time.Second
	waitPollInterval      = time.Millisecond
	testLongTextLength    = 161
	testEnqueueRecipients = 2
)

// newTestRepository builds a Repository over a fresh in-memory SQLite
// database with the embedded goose migration applied.
func newTestRepository(t *testing.T) *Repository {
	t.Helper()

	sqldb, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqldb.SetMaxOpenConns(testMaxConns)
	sqldb.SetMaxIdleConns(testMaxConns)
	t.Cleanup(func() {
		if closeErr := sqldb.Close(); closeErr != nil {
			t.Errorf("close sqlite: %v", closeErr)
		}
	})

	provider, providerErr := goose.NewProvider(database.DialectSQLite3, sqldb, goosefx.Storage(migrations.FS))
	if providerErr != nil {
		t.Fatalf("init goose provider: %v", providerErr)
	}
	if _, upErr := provider.Up(context.Background()); upErr != nil {
		t.Fatalf("apply migrations: %v", upErr)
	}

	return NewRepository(bunfx.New(sqldb, sqlitedialect.New(), zap.NewNop()))
}

// testTime returns a fixed UTC instant offset by duration; second precision
// survives SQLite DATETIME roundtrips.
func testTime(offset time.Duration) time.Time {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	return base.Add(offset)
}

func testConfig() Config {
	return Config{
		PollInterval: testPollInterval,
		DeviceID:     "device-under-test",
	}
}

// newTestMetrics builds the full Metrics struct with plain Prometheus
// constructors so tests never touch the global promauto registry
// (duplicate-registration panics), mirroring the modem test pattern.
func newTestMetrics() *Metrics {
	return &Metrics{
		EnqueuedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "at_gateway_messages_enqueued_total",
			Help: "total messages enqueued (test)",
		}),
		SentTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "at_gateway_messages_sent_total",
			Help: "total messages sent (test)",
		}),
		FailedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "at_gateway_messages_failed_total",
			Help: "total messages failed (test)",
		}),
		CancelledTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "at_gateway_messages_cancelled_total",
			Help: "total messages cancelled (test)",
		}),
	}
}

func newTestMessage(id string, createdAt time.Time, phone string) *Message {
	return &Message{
		ID:                 id,
		DeviceID:           "device-under-test",
		State:              StatePending,
		IsHashed:           false,
		IsEncrypted:        false,
		TextMessage:        "hello world",
		SimNumber:          nil,
		WithDeliveryReport: false,
		Priority:           0,
		Recipients:         []string{phone},
		States:             []StateChange{{State: StatePending, At: createdAt}},
		ErrorMessage:       nil,
		CreatedAt:          createdAt,
		UpdatedAt:          createdAt,
		ProcessedAt:        nil,
		SentAt:             nil,
		FailedAt:           nil,
	}
}

// newEnqueueRequest builds a fully-populated wire DTO for Enqueue.
func newEnqueueRequest(text string, phones []string) smsgateway.Message {
	return smsgateway.Message{
		ID:                 "",
		DeviceID:           "",
		Message:            "",
		TextMessage:        &smsgateway.TextMessage{Text: text},
		DataMessage:        nil,
		PhoneNumbers:       phones,
		IsEncrypted:        false,
		SimNumber:          nil,
		WithDeliveryReport: nil,
		Priority:           0,
		TTL:                nil,
		ValidUntil:         nil,
		ScheduleAt:         nil,
	}
}

// fakeSender is the concrete sender stub: it records calls and returns the
// configured ref/error, optionally blocking on channels for the
// cancel-during-send scenario.
type fakeSender struct {
	ref int
	err error

	mu    sync.Mutex
	calls []string

	block       chan struct{}
	started     chan struct{}
	startedOnce sync.Once
}

func (f *fakeSender) SendSMS(_ context.Context, phoneNumber, _ string) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, phoneNumber)
	f.mu.Unlock()

	if f.started != nil {
		f.startedOnce.Do(func() { close(f.started) })
	}
	if f.block != nil {
		<-f.block
	}

	return f.ref, f.err
}

func (f *fakeSender) phones() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.calls...)
}

// newService wires a Service over the given repo with the fake sender bound
// to the send slot (the real binding is *modem.Service.SendSMS in New).
func newService(repo *Repository, sender *fakeSender, metrics *Metrics) *Service {
	return &Service{
		config:  testConfig(),
		repo:    repo,
		sendSMS: sender.SendSMS,
		metrics: metrics,
		logger:  zap.NewNop(),
	}
}

// startWorker runs svc.Run in a goroutine and guarantees it stops: cleanup
// cancels the context and waits for Run to return nil.
func startWorker(t *testing.T, svc *Service) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned error: %v", err)
			}
		case <-time.After(testWaitTimeout):
			t.Error("Run did not stop after context cancellation")
		}
	})
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(testWaitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(waitPollInterval)
	}
	t.Fatal("condition not met before timeout")
}

func TestEnqueue_RejectsDataMessage(t *testing.T) {
	repo := newTestRepository(t)
	svc := newService(repo, &fakeSender{ref: 1}, newTestMetrics())

	request := newEnqueueRequest("hello", []string{"+15550000001"})
	request.DataMessage = &smsgateway.DataMessage{Data: "AAECAw==", Port: 1}

	_, err := svc.Enqueue(context.Background(), request)
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("Enqueue error = %v, want ErrNotSupported", err)
	}

	assertEmptyQueue(t, repo)
}

func TestEnqueue_RejectsInvalidText(t *testing.T) {
	cases := []struct {
		name string
		msg  smsgateway.Message
	}{
		{name: "missing text message", msg: smsgateway.Message{PhoneNumbers: []string{"+15550000001"}}},
		{name: "empty text", msg: newEnqueueRequest("", []string{"+15550000001"})},
		{
			name: "non-ASCII text",
			msg:  newEnqueueRequest("\u043f\u0440\u0438\u0432\u0435\u0442", []string{"+15550000001"}),
		},
		{
			name: "text too long",
			msg:  newEnqueueRequest(strings.Repeat("a", testLongTextLength), []string{"+15550000001"}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newTestRepository(t)
			svc := newService(repo, &fakeSender{ref: 1}, newTestMetrics())

			_, err := svc.Enqueue(context.Background(), tc.msg)
			if !errors.Is(err, ErrInvalidText) {
				t.Fatalf("Enqueue error = %v, want ErrInvalidText", err)
			}

			assertEmptyQueue(t, repo)
		})
	}
}

func TestEnqueue_RejectsEmptyPhoneNumbers(t *testing.T) {
	cases := []struct {
		name   string
		phones []string
	}{
		{name: "no phone numbers", phones: nil},
		{name: "empty phone number", phones: []string{""}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newTestRepository(t)
			svc := newService(repo, &fakeSender{ref: 1}, newTestMetrics())

			_, err := svc.Enqueue(context.Background(), newEnqueueRequest("hello", tc.phones))
			if !errors.Is(err, ErrInvalidPhoneNumbers) {
				t.Fatalf("Enqueue error = %v, want ErrInvalidPhoneNumbers", err)
			}

			assertEmptyQueue(t, repo)
		})
	}
}

func TestEnqueue_PersistsPendingAndReturnsDTO(t *testing.T) {
	repo := newTestRepository(t)
	metrics := newTestMetrics()
	svc := newService(repo, &fakeSender{ref: 1}, metrics)
	ctx := context.Background()

	phones := []string{"+15550000001", "+15550000002"}
	state, err := svc.Enqueue(ctx, newEnqueueRequest("hello world", phones))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if state.ID == "" {
		t.Error("DTO ID is empty")
	}
	if state.DeviceID != "device-under-test" {
		t.Errorf("DTO DeviceID = %q, want device-under-test", state.DeviceID)
	}
	if state.State != smsgateway.ProcessingStatePending {
		t.Errorf("DTO State = %q, want Pending", state.State)
	}
	if state.IsHashed || state.IsEncrypted {
		t.Error("DTO IsHashed/IsEncrypted = true, want false")
	}
	if state.TextMessage != nil || state.DataMessage != nil || state.HashedMessage != nil {
		t.Error("DTO content fields must be nil (content withheld in MVP)")
	}
	if len(state.Recipients) != testEnqueueRecipients {
		t.Fatalf("DTO Recipients = %d entries, want %d", len(state.Recipients), testEnqueueRecipients)
	}
	for i, phone := range phones {
		recipient := state.Recipients[i]
		if recipient.PhoneNumber != phone {
			t.Errorf("DTO Recipients[%d].PhoneNumber = %q, want %q", i, recipient.PhoneNumber, phone)
		}
		if recipient.State != smsgateway.ProcessingStatePending {
			t.Errorf("DTO Recipients[%d].State = %q, want Pending", i, recipient.State)
		}
		if recipient.Error != nil {
			t.Errorf("DTO Recipients[%d].Error = %v, want nil", i, recipient.Error)
		}
	}
	if len(state.States) != 1 {
		t.Fatalf("DTO States = %v, want exactly one entry", state.States)
	}
	if _, ok := state.States[string(StatePending)]; !ok {
		t.Errorf("DTO States has no Pending entry: %v", state.States)
	}

	persisted, getErr := repo.GetByID(ctx, state.ID)
	if getErr != nil {
		t.Fatalf("GetByID after Enqueue: %v", getErr)
	}
	if persisted.State != StatePending || persisted.TextMessage != "hello world" {
		t.Errorf("persisted = %+v, want Pending with text hello world", persisted)
	}
	if len(persisted.States) != 1 || persisted.States[0].State != StatePending {
		t.Errorf("persisted States = %+v, want single Pending entry", persisted.States)
	}

	if got := testutil.ToFloat64(metrics.EnqueuedTotal); got != 1 {
		t.Errorf("EnqueuedTotal = %v, want 1", got)
	}
}

func TestEnqueue_UsesDeprecatedMessageField(t *testing.T) {
	repo := newTestRepository(t)
	svc := newService(repo, &fakeSender{ref: 1}, newTestMetrics())

	request := newEnqueueRequest("", []string{"+15550000001"})
	request.TextMessage = nil
	request.Message = "legacy text"

	state, err := svc.Enqueue(context.Background(), request)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	persisted, getErr := repo.GetByID(context.Background(), state.ID)
	if getErr != nil {
		t.Fatalf("GetByID: %v", getErr)
	}
	if persisted.TextMessage != "legacy text" {
		t.Errorf("TextMessage = %q, want legacy text", persisted.TextMessage)
	}
}

func TestGet_ReturnsDTO(t *testing.T) {
	repo := newTestRepository(t)
	svc := newService(repo, &fakeSender{ref: 1}, newTestMetrics())
	ctx := context.Background()

	enqueued, err := svc.Enqueue(ctx, newEnqueueRequest("hello", []string{"+15550000001"}))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if updateErr := repo.UpdateState(ctx, enqueued.ID, StateSent, nil); updateErr != nil {
		t.Fatalf("UpdateState Sent: %v", updateErr)
	}

	state, getErr := svc.Get(ctx, enqueued.ID)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if state.ID != enqueued.ID || state.State != smsgateway.ProcessingStateSent {
		t.Errorf("Get = %+v, want ID %s in Sent state", state, enqueued.ID)
	}
	if state.Recipients[0].State != smsgateway.ProcessingStateSent {
		t.Errorf("Recipients[0].State = %q, want Sent", state.Recipients[0].State)
	}
	if _, ok := state.States[string(StateSent)]; !ok {
		t.Errorf("States lacks Sent entry: %v", state.States)
	}
}

func TestGet_NotFound(t *testing.T) {
	repo := newTestRepository(t)
	svc := newService(repo, &fakeSender{ref: 1}, newTestMetrics())

	_, err := svc.Get(context.Background(), "missing-id")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestList_MapsDTOsAndTotal(t *testing.T) {
	repo := newTestRepository(t)
	svc := newService(repo, &fakeSender{ref: 1}, newTestMetrics())
	ctx := context.Background()

	first, err := svc.Enqueue(ctx, newEnqueueRequest("first", []string{"+15550000001"}))
	if err != nil {
		t.Fatalf("Enqueue first: %v", err)
	}
	second, err := svc.Enqueue(ctx, newEnqueueRequest("second", []string{"+15550000002", "+15550000003"}))
	if err != nil {
		t.Fatalf("Enqueue second: %v", err)
	}
	if updateErr := repo.UpdateState(ctx, first.ID, StateSent, nil); updateErr != nil {
		t.Fatalf("UpdateState Sent: %v", updateErr)
	}

	all, total, listErr := svc.List(ctx, ListFilter{Limit: 0, Offset: 0, State: nil, Order: SortAsc})
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if total != testEnqueueRecipients || len(all) != testEnqueueRecipients {
		t.Fatalf("List = %d rows (total %d), want %d", len(all), total, testEnqueueRecipients)
	}

	var sentDTO *smsgateway.MessageState
	for i := range all {
		switch all[i].ID {
		case first.ID:
			if all[i].State != smsgateway.ProcessingStateSent {
				t.Errorf("first DTO State = %q, want Sent", all[i].State)
			}
			sentDTO = &all[i]
		case second.ID:
			if all[i].State != smsgateway.ProcessingStatePending {
				t.Errorf("second DTO State = %q, want Pending", all[i].State)
			}
		}
	}
	if sentDTO == nil {
		t.Fatal("List did not return the sent message")
	}

	pendingState := StatePending
	pending, total, listErr := svc.List(ctx, ListFilter{Limit: 0, Offset: 0, State: &pendingState, Order: SortAsc})
	if listErr != nil {
		t.Fatalf("List pending: %v", listErr)
	}
	if total != 1 || len(pending) != 1 || pending[0].ID != second.ID {
		t.Errorf("pending filter = %+v (total %d), want only %s", pending, total, second.ID)
	}
}

func TestCancel_TransitionsAndErrors(t *testing.T) {
	repo := newTestRepository(t)
	metrics := newTestMetrics()
	svc := newService(repo, &fakeSender{ref: 1}, metrics)
	ctx := context.Background()

	enqueued, err := svc.Enqueue(ctx, newEnqueueRequest("hello", []string{"+15550000001"}))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	cancelled, cancelErr := svc.Cancel(ctx, enqueued.ID)
	if cancelErr != nil {
		t.Fatalf("Cancel: %v", cancelErr)
	}
	if cancelled.State != smsgateway.ProcessingStateCancelled {
		t.Errorf("Cancel state = %q, want Cancelled", cancelled.State)
	}
	if _, ok := cancelled.States[string(StateCancelled)]; !ok {
		t.Errorf("States lacks Cancelled entry: %v", cancelled.States)
	}
	persisted, getErr := repo.GetByID(ctx, enqueued.ID)
	if getErr != nil {
		t.Fatalf("GetByID after Cancel: %v", getErr)
	}
	if persisted.State != StateCancelled || persisted.ProcessedAt == nil {
		t.Errorf("persisted = %+v, want Cancelled with ProcessedAt", persisted)
	}
	if got := testutil.ToFloat64(metrics.CancelledTotal); got != 1 {
		t.Errorf("CancelledTotal = %v, want 1", got)
	}

	if _, repeatErr := svc.Cancel(ctx, enqueued.ID); !errors.Is(repeatErr, ErrNotPending) {
		t.Fatalf("second Cancel error = %v, want ErrNotPending", repeatErr)
	}
	if _, missingErr := svc.Cancel(ctx, "missing-id"); !errors.Is(missingErr, ErrNotFound) {
		t.Fatalf("Cancel missing error = %v, want ErrNotFound", missingErr)
	}
}

func TestRun_ProcessesFIFOToSent(t *testing.T) {
	repo := newTestRepository(t)
	metrics := newTestMetrics()
	sender := &fakeSender{ref: 42}
	svc := newService(repo, sender, metrics)
	ctx := context.Background()

	// Seed two pending messages with shuffled insertion order; the worker
	// must send the oldest first (FIFO by created_at).
	older := newTestMessage("fifo-0001", testTime(time.Minute), "+15550000001")
	newer := newTestMessage("fifo-0002", testTime(0), "+15550000002")
	for _, message := range []*Message{older, newer} {
		if _, createErr := repo.Create(ctx, message); createErr != nil {
			t.Fatalf("Create %s: %v", message.ID, createErr)
		}
	}

	startWorker(t, svc)
	waitFor(t, func() bool {
		first, firstErr := repo.GetByID(ctx, older.ID)
		second, secondErr := repo.GetByID(ctx, newer.ID)
		if firstErr != nil || secondErr != nil {
			return false
		}

		return first.State == StateSent && second.State == StateSent
	})

	if got := testutil.ToFloat64(metrics.SentTotal); got != testEnqueueRecipients {
		t.Errorf("SentTotal = %v, want %d", got, testEnqueueRecipients)
	}
	gotPhones := sender.phones()
	if len(gotPhones) != testEnqueueRecipients || gotPhones[0] != newer.Recipients[0] ||
		gotPhones[1] != older.Recipients[0] {
		t.Errorf("send order = %v, want [%s %s]", gotPhones, newer.Recipients[0], older.Recipients[0])
	}
	for _, message := range []*Message{older, newer} {
		current, getErr := repo.GetByID(ctx, message.ID)
		if getErr != nil {
			t.Fatalf("GetByID %s: %v", message.ID, getErr)
		}
		if current.SentAt == nil {
			t.Errorf("message %s has no SentAt", message.ID)
		}
	}
}

func TestRun_SendsToAllRecipients(t *testing.T) {
	repo := newTestRepository(t)
	metrics := newTestMetrics()
	sender := &fakeSender{ref: 7}
	svc := newService(repo, sender, metrics)
	ctx := context.Background()

	enqueued, err := svc.Enqueue(ctx, newEnqueueRequest("hello", []string{"+15550000001", "+15550000002"}))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	startWorker(t, svc)
	waitFor(t, func() bool {
		current, getErr := repo.GetByID(ctx, enqueued.ID)

		return getErr == nil && current.State == StateSent
	})

	gotPhones := sender.phones()
	if len(gotPhones) != testEnqueueRecipients {
		t.Fatalf("send calls = %v, want %d", gotPhones, testEnqueueRecipients)
	}
	if got := testutil.ToFloat64(metrics.SentTotal); got != 1 {
		t.Errorf("SentTotal = %v, want 1 (per message, not per recipient)", got)
	}
}

func TestRun_SendFailureMarksFailed(t *testing.T) {
	repo := newTestRepository(t)
	metrics := newTestMetrics()
	sender := &fakeSender{ref: 0, err: errors.New("modem timeout")}
	svc := newService(repo, sender, metrics)
	ctx := context.Background()

	enqueued, err := svc.Enqueue(ctx, newEnqueueRequest("hello", []string{"+15550000001"}))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	startWorker(t, svc)
	waitFor(t, func() bool {
		current, getErr := repo.GetByID(ctx, enqueued.ID)

		return getErr == nil && current.State == StateFailed
	})

	current, getErr := repo.GetByID(ctx, enqueued.ID)
	if getErr != nil {
		t.Fatalf("GetByID: %v", getErr)
	}
	if current.ErrorMessage == nil || *current.ErrorMessage != "modem timeout" {
		t.Errorf("ErrorMessage = %v, want modem timeout", current.ErrorMessage)
	}
	if current.FailedAt == nil {
		t.Error("FailedAt not stamped on failure")
	}
	if got := testutil.ToFloat64(metrics.FailedTotal); got != 1 {
		t.Errorf("FailedTotal = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.SentTotal); got != 0 {
		t.Errorf("SentTotal = %v, want 0", got)
	}
}

func TestRun_SkipsCancelledMessages(t *testing.T) {
	repo := newTestRepository(t)
	metrics := newTestMetrics()
	sender := &fakeSender{ref: 1}
	svc := newService(repo, sender, metrics)
	ctx := context.Background()

	cancelled := newTestMessage("cancelled-01", testTime(0), "+15550000001")
	pending := newTestMessage("pending-0001", testTime(time.Minute), "+15550000002")
	for _, message := range []*Message{cancelled, pending} {
		if _, createErr := repo.Create(ctx, message); createErr != nil {
			t.Fatalf("Create %s: %v", message.ID, createErr)
		}
	}
	if _, cancelErr := repo.Cancel(ctx, cancelled.ID); cancelErr != nil {
		t.Fatalf("Cancel: %v", cancelErr)
	}

	startWorker(t, svc)
	waitFor(t, func() bool {
		current, getErr := repo.GetByID(ctx, pending.ID)

		return getErr == nil && current.State == StateSent
	})

	gotPhones := sender.phones()
	if len(gotPhones) != 1 || gotPhones[0] != pending.Recipients[0] {
		t.Errorf("send calls = %v, want only [%s]", gotPhones, pending.Recipients[0])
	}
	current, getErr := repo.GetByID(ctx, cancelled.ID)
	if getErr != nil {
		t.Fatalf("GetByID cancelled: %v", getErr)
	}
	if current.State != StateCancelled {
		t.Errorf("cancelled message state = %q, want Cancelled", current.State)
	}
	if got := testutil.ToFloat64(metrics.SentTotal); got != 1 {
		t.Errorf("SentTotal = %v, want 1", got)
	}
}

func TestRun_CancelDuringSendKeepsCancelled(t *testing.T) {
	repo := newTestRepository(t)
	metrics := newTestMetrics()
	sender := &fakeSender{ref: 1, block: make(chan struct{}), started: make(chan struct{})}
	svc := newService(repo, sender, metrics)
	ctx := context.Background()

	enqueued, err := svc.Enqueue(ctx, newEnqueueRequest("hello", []string{"+15550000001"}))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	startWorker(t, svc)
	<-sender.started // the send is in flight on the modem

	if _, cancelErr := svc.Cancel(ctx, enqueued.ID); cancelErr != nil {
		t.Fatalf("Cancel during send: %v", cancelErr)
	}
	close(sender.block) // let the send complete

	waitFor(t, func() bool {
		current, getErr := repo.GetByID(ctx, enqueued.ID)

		return getErr == nil && current.State == StateCancelled
	})

	current, getErr := repo.GetByID(ctx, enqueued.ID)
	if getErr != nil {
		t.Fatalf("GetByID: %v", getErr)
	}
	if current.State != StateCancelled {
		t.Errorf("state = %q, want Cancelled (Cancel must win over the in-flight send)", current.State)
	}
	if got := testutil.ToFloat64(metrics.SentTotal); got != 0 {
		t.Errorf("SentTotal = %v, want 0", got)
	}
}

func TestRun_StopsOnContextCancellation(t *testing.T) {
	repo := newTestRepository(t)
	svc := newService(repo, &fakeSender{ref: 1}, newTestMetrics())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	cancel()

	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("Run returned error: %v", runErr)
		}
	case <-time.After(testWaitTimeout):
		t.Fatal("Run did not stop after context cancellation")
	}
}

// assertEmptyQueue verifies nothing was persisted by a rejected Enqueue.
func assertEmptyQueue(t *testing.T, repo *Repository) {
	t.Helper()

	rows, total, err := repo.List(context.Background(), ListFilter{Limit: 0, Offset: 0, State: nil, Order: SortAsc})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 0 || len(rows) != 0 {
		t.Errorf("queue not empty after rejected Enqueue: %d rows", len(rows))
	}
}
