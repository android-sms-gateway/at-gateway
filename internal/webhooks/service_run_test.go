package webhooks_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/uptrace/bun"
	"go.uber.org/zap"
)

const (
	testSigningKey = "worker-test-key"
	testPayload    = `{"id":"queue-1","payload":{"text":"  raw bytes  "}}`
)

// TestSignPayloadGolden pins DEC-3 byte truth before the worker tests: the
// signature is bare lowercase hex HMAC-SHA256 over body concatenated with the
// unix-seconds timestamp, with no prefix or separator.
func TestSignPayloadGolden(t *testing.T) {
	const timestamp = "1760000000"
	const want = "e5e383e00d5721f1f0f6d95b7363c370ee242f8e671c9f8615e6e4ab5a404889"

	got := webhooks.SignPayloadForTest("0123456789abcdef", `{"id":"queue-1","payload":{"messageId":"m-1"}}`, timestamp)
	if got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

type runFixture struct {
	service *webhooks.Service
	repo    *webhooks.Repository
	db      *bun.DB
	metrics *webhooks.Metrics
}

func newRunFixture(t *testing.T, config webhooks.Config) runFixture {
	t.Helper()

	repo, _, db := newPersistence(t)
	storageSvc, err := storage.NewService(
		storage.Config{Path: filepath.Join(t.TempDir(), "storage.json")},
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create storage service: %v", err)
	}

	devicesSvc := devices.NewService(devices.Config{Name: "test-device"}, storageSvc, zap.NewNop())
	metrics := newTestMetrics()
	service, err := webhooks.NewService(config, repo, devicesSvc, storageSvc, metrics, zap.NewNop())
	if err != nil {
		t.Fatalf("create webhooks service: %v", err)
	}

	return runFixture{
		service: service,
		repo:    repo,
		db:      db,
		metrics: metrics,
	}
}

func runConfig(retryCount int) webhooks.Config {
	return webhooks.Config{
		RetryCount: retryCount,
		SigningKey: testSigningKey,
		Queue: webhooks.QueueConfig{
			BatchSize:              20,
			RequestTimeout:         500 * time.Millisecond,
			DialTimeout:            100 * time.Millisecond,
			RetryBaseDelay:         20 * time.Millisecond,
			IdleDelay:              5 * time.Millisecond,
			StuckProcessingTimeout: 100 * time.Millisecond,
			CleanupRetention:       24 * time.Hour,
		},
	}
}

type capturedRequest struct {
	body        []byte
	timestamp   string
	signature   string
	contentType string
	receivedAt  time.Time
}

type requestLog struct {
	mu       sync.Mutex
	requests []capturedRequest
}

func (l *requestLog) add(request capturedRequest) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.requests = append(l.requests, request)
	return len(l.requests)
}

func (l *requestLog) snapshot() []capturedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()

	requests := make([]capturedRequest, len(l.requests))
	copy(requests, l.requests)
	return requests
}

func newRequestServer(
	t *testing.T,
	log *requestLog,
	status func(int) int,
	beforeResponse func(int),
) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}

		index := log.add(capturedRequest{
			body:        body,
			timestamp:   request.Header.Get("X-Timestamp"),
			signature:   request.Header.Get("X-Signature"),
			contentType: request.Header.Get("Content-Type"),
			receivedAt:  time.Now(),
		})
		if beforeResponse != nil {
			beforeResponse(index)
		}
		writer.WriteHeader(status(index))
	}))
	t.Cleanup(server.Close)
	return server
}

func newTransportErrorServer(t *testing.T, log *requestLog) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return
		}
		log.add(capturedRequest{
			body:        body,
			timestamp:   request.Header.Get("X-Timestamp"),
			signature:   request.Header.Get("X-Signature"),
			contentType: request.Header.Get("Content-Type"),
			receivedAt:  time.Now(),
		})

		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			return
		}
		connection, _, hijackErr := hijacker.Hijack()
		if hijackErr != nil {
			return
		}
		_ = connection.Close()
	}))
	t.Cleanup(server.Close)
	return server
}

func verifyRequests(t *testing.T, log *requestLog, wantCount int) {
	t.Helper()

	requests := log.snapshot()
	if len(requests) != wantCount {
		t.Fatalf("POST count = %d, want %d", len(requests), wantCount)
	}
	for index, request := range requests {
		if string(request.body) != testPayload {
			t.Errorf("POST %d body = %q, want exact stored payload %q", index+1, request.body, testPayload)
		}
		if request.contentType != "application/json" {
			t.Errorf("POST %d Content-Type = %q, want application/json", index+1, request.contentType)
		}
		if request.timestamp == "" {
			t.Errorf("POST %d has no X-Timestamp", index+1)
		}
		if request.signature == "" {
			t.Errorf("POST %d has no X-Signature", index+1)
		}
		if request.signature != strings.ToLower(request.signature) || len(request.signature) != sha256.Size*2 {
			t.Errorf("POST %d signature = %q, want lowercase 64-hex", index+1, request.signature)
		}

		mac := hmac.New(sha256.New, []byte(testSigningKey))
		_, _ = mac.Write(request.body)
		_, _ = mac.Write([]byte(request.timestamp))
		wantSignature := hex.EncodeToString(mac.Sum(nil))
		if request.signature != wantSignature {
			t.Errorf("POST %d signature = %q, want %q", index+1, request.signature, wantSignature)
		}
	}
}

type workerHandle struct {
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

func startWorker(t *testing.T, service *webhooks.Service) *workerHandle {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	handle := &workerHandle{
		cancel: cancel,
		done:   make(chan error, 1),
	}
	go func() {
		handle.done <- service.Run(ctx)
	}()
	t.Cleanup(func() {
		handle.stop(t)
	})
	return handle
}

func (h *workerHandle) stop(t *testing.T) {
	t.Helper()

	h.once.Do(func() {
		h.cancel()
		select {
		case err := <-h.done:
			if err != nil {
				t.Errorf("worker Run returned error: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("worker Run did not stop after context cancellation")
		}
	})
}

func waitForStatus(t *testing.T, fixture runFixture, id string, want webhooks.QueueStatus) webhooks.QueueItem {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		item := loadQueueItem(t, fixture.db, id)
		if item.Status == want {
			return item
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue item %s status = %q, want %q", id, item.Status, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForCount(t *testing.T, fixture runFixture, id string, want int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		got := countQueueItems(t, fixture.db, id)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue item %s count = %d, want %d", id, got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func enqueueRunItem(t *testing.T, fixture runFixture, id, url string, createdAt, nextAttempt time.Time) {
	t.Helper()

	item := newQueueItem(id, "webhook-1", url, testPayload, createdAt, nextAttempt)
	if err := fixture.repo.Enqueue(context.Background(), item); err != nil {
		t.Fatalf("enqueue queue item %s: %v", id, err)
	}
}

func TestRun_DeliversStoredEnvelopeAndSignsEveryPost(t *testing.T) {
	log := &requestLog{}
	server := newRequestServer(t, log, func(int) int { return http.StatusOK }, nil)
	fixture := newRunFixture(t, runConfig(1))
	now := time.Now().UTC()
	enqueueRunItem(t, fixture, "success", server.URL, now.Add(-time.Hour), now.Add(-time.Hour))

	worker := startWorker(t, fixture.service)
	waitForStatus(t, fixture, "success", webhooks.QueueStatusCompleted)
	worker.stop(t)

	verifyRequests(t, log, 1)
	if got := webhooks.CounterValue(fixture.metrics, "at_gateway_webhooks_delivered_total"); got != 1 {
		t.Fatalf("delivered metric = %v, want 1", got)
	}
	if got := webhooks.CounterValue(fixture.metrics, "at_gateway_webhooks_failed_total"); got != 0 {
		t.Fatalf("failed metric = %v, want 0", got)
	}
	if got := webhooks.CounterValue(fixture.metrics, "at_gateway_webhooks_permanently_failed_total"); got != 0 {
		t.Fatalf("permanently failed metric = %v, want 0", got)
	}
}

func TestRun_FailedThenSuccessfulRetry(t *testing.T) {
	log := &requestLog{}
	firstRequest := make(chan struct{})
	releaseFirst := make(chan struct{})
	server := newRequestServer(t, log, func(index int) int {
		if index == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	}, func(index int) {
		if index == 1 {
			close(firstRequest)
			<-releaseFirst
		}
	})
	config := runConfig(1)
	config.Queue.RetryBaseDelay = 200 * time.Millisecond
	fixture := newRunFixture(t, config)
	now := time.Now().UTC()
	enqueueRunItem(t, fixture, "retry-success", server.URL, now.Add(-time.Hour), now.Add(-time.Hour))

	worker := startWorker(t, fixture.service)
	select {
	case <-firstRequest:
	case <-time.After(time.Second):
		t.Fatal("worker did not issue the first POST")
	}
	close(releaseFirst)

	failed := waitForStatus(t, fixture, "retry-success", webhooks.QueueStatusFailed)
	if failed.RetryCount != 1 {
		t.Fatalf("failed queue item retry_count = %d, want 1", failed.RetryCount)
	}
	if failed.LastError == nil || !strings.Contains(*failed.LastError, "500") {
		t.Fatalf("failed queue item last_error = %v, want status 500", failed.LastError)
	}

	completed := waitForStatus(t, fixture, "retry-success", webhooks.QueueStatusCompleted)
	if completed.RetryCount != 1 {
		t.Fatalf("completed queue item retry_count = %d, want 1", completed.RetryCount)
	}
	worker.stop(t)

	verifyRequests(t, log, 2)
	if got := webhooks.CounterValue(fixture.metrics, "at_gateway_webhooks_delivered_total"); got != 1 {
		t.Fatalf("delivered metric = %v, want 1", got)
	}
	if got := webhooks.CounterValue(fixture.metrics, "at_gateway_webhooks_failed_total"); got != 1 {
		t.Fatalf("failed metric = %v, want 1", got)
	}
}

func TestRun_NonSuccessStatusesShareRetryPolicy(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			log := &requestLog{}
			server := newRequestServer(t, log, func(int) int { return status }, nil)
			fixture := newRunFixture(t, runConfig(1))
			now := time.Now().UTC()
			enqueueRunItem(t, fixture, "non-success", server.URL, now.Add(-time.Hour), now.Add(-time.Hour))

			worker := startWorker(t, fixture.service)
			waitForStatus(t, fixture, "non-success", webhooks.QueueStatusPermanentlyFailed)
			worker.stop(t)

			if got := len(log.snapshot()); got != 2 {
				t.Fatalf("POST count = %d, want RetryCount+1 = 2", got)
			}
			item := loadQueueItem(t, fixture.db, "non-success")
			if item.RetryCount != 1 {
				t.Fatalf("permanently failed retry_count = %d, want 1", item.RetryCount)
			}
			if item.LastError == nil || !strings.Contains(*item.LastError, http.StatusText(status)) {
				t.Fatalf("last_error = %v, want status text %q", item.LastError, http.StatusText(status))
			}
			verifyRequests(t, log, 2)
		})
	}
}

func TestRun_RetryScheduleUsesClaimTimeExponential(t *testing.T) {
	config := runConfig(2)
	config.Queue.RetryBaseDelay = 200 * time.Millisecond
	log := &requestLog{}
	firstRequest := make(chan struct{})
	secondRequest := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	server := newRequestServer(t, log, func(int) int {
		return http.StatusInternalServerError
	}, func(index int) {
		switch index {
		case 1:
			close(firstRequest)
			<-releaseFirst
		case 2:
			close(secondRequest)
			<-releaseSecond
		}
	})
	fixture := newRunFixture(t, config)
	now := time.Now().UTC()
	enqueueRunItem(t, fixture, "retry-schedule", server.URL, now.Add(-time.Hour), now.Add(-time.Hour))

	worker := startWorker(t, fixture.service)
	select {
	case <-firstRequest:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not issue the first retry-schedule POST")
	}
	close(releaseFirst)

	firstFailure := waitForStatus(t, fixture, "retry-schedule", webhooks.QueueStatusFailed)
	firstRecords := log.snapshot()
	if len(firstRecords) != 1 {
		t.Fatalf("POST count before second retry = %d, want 1", len(firstRecords))
	}
	firstDelay := firstFailure.NextAttempt.Sub(firstRecords[0].receivedAt)
	if firstDelay < config.Queue.RetryBaseDelay ||
		firstDelay > config.Queue.RetryBaseDelay+config.Queue.RetryBaseDelay/2 {
		t.Fatalf("first retry delay = %s, want about %s", firstDelay, config.Queue.RetryBaseDelay)
	}

	select {
	case <-secondRequest:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not issue the second retry-schedule POST")
	}
	close(releaseSecond)

	secondFailure := waitForStatus(t, fixture, "retry-schedule", webhooks.QueueStatusFailed)
	secondRecords := log.snapshot()
	if len(secondRecords) != 2 {
		t.Fatalf("POST count after second retry = %d, want 2", len(secondRecords))
	}
	secondDelay := secondFailure.NextAttempt.Sub(secondRecords[1].receivedAt)
	wantSecondDelay := 2 * config.Queue.RetryBaseDelay
	if secondDelay < wantSecondDelay || secondDelay > wantSecondDelay+config.Queue.RetryBaseDelay/2 {
		t.Fatalf("second retry delay = %s, want about %s", secondDelay, wantSecondDelay)
	}
	worker.stop(t)
	verifyRequests(t, log, 2)
}

func TestRun_PermanentFailureStopsAtRetryCap(t *testing.T) {
	log := &requestLog{}
	server := newRequestServer(t, log, func(int) int { return http.StatusServiceUnavailable }, nil)
	fixture := newRunFixture(t, runConfig(2))
	now := time.Now().UTC()
	enqueueRunItem(t, fixture, "permanent", server.URL, now.Add(-time.Hour), now.Add(-time.Hour))

	worker := startWorker(t, fixture.service)
	waitForStatus(t, fixture, "permanent", webhooks.QueueStatusPermanentlyFailed)
	worker.stop(t)

	if got := len(log.snapshot()); got != 3 {
		t.Fatalf("POST count = %d, want RetryCount+1 = 3", got)
	}
	item := loadQueueItem(t, fixture.db, "permanent")
	if item.RetryCount != 2 {
		t.Fatalf("permanent retry_count = %d, want 2", item.RetryCount)
	}
	if got := webhooks.CounterValue(fixture.metrics, "at_gateway_webhooks_failed_total"); got != 3 {
		t.Fatalf("failed metric = %v, want 3", got)
	}
	if got := webhooks.CounterValue(fixture.metrics, "at_gateway_webhooks_permanently_failed_total"); got != 1 {
		t.Fatalf("permanently failed metric = %v, want 1", got)
	}
	verifyRequests(t, log, 3)
}

func TestRun_NetworkErrorIsRetryable(t *testing.T) {
	log := &requestLog{}
	server := newTransportErrorServer(t, log)
	fixture := newRunFixture(t, runConfig(1))
	now := time.Now().UTC()
	enqueueRunItem(t, fixture, "network-error", server.URL, now.Add(-time.Hour), now.Add(-time.Hour))

	worker := startWorker(t, fixture.service)
	waitForStatus(t, fixture, "network-error", webhooks.QueueStatusPermanentlyFailed)
	worker.stop(t)

	if got := len(log.snapshot()); got != 2 {
		t.Fatalf("POST count = %d, want RetryCount+1 = 2", got)
	}
	item := loadQueueItem(t, fixture.db, "network-error")
	if item.RetryCount != 1 {
		t.Fatalf("network failure retry_count = %d, want 1", item.RetryCount)
	}
	if item.LastError == nil || *item.LastError == "" {
		t.Fatal("network failure last_error is empty")
	}
	verifyRequests(t, log, 2)
}

func TestRun_RecoversStuckProcessingBeforePolling(t *testing.T) {
	config := runConfig(0)
	config.Queue.BatchSize = 0
	config.Queue.IdleDelay = 100 * time.Millisecond
	config.Queue.StuckProcessingTimeout = 20 * time.Millisecond
	fixture := newRunFixture(t, config)
	now := time.Now().UTC()
	item := newQueueItem("stuck", "webhook-1", "", testPayload, now.Add(-time.Hour), now.Add(-time.Hour))
	item.Status = webhooks.QueueStatusProcessing
	if err := fixture.repo.Enqueue(context.Background(), item); err != nil {
		t.Fatalf("enqueue stuck item: %v", err)
	}

	worker := startWorker(t, fixture.service)
	recovered := waitForStatus(t, fixture, "stuck", webhooks.QueueStatusPending)
	if recovered.RetryCount != 0 {
		t.Fatalf("recovered retry_count = %d, want 0", recovered.RetryCount)
	}
	worker.stop(t)
}

func TestRun_CleansOldTerminalRowsWhenIdle(t *testing.T) {
	config := runConfig(0)
	config.Queue.CleanupRetention = 20 * time.Millisecond
	config.Queue.IdleDelay = 5 * time.Millisecond
	fixture := newRunFixture(t, config)
	now := time.Now().UTC()
	oldTime := now.Add(-time.Hour)
	oldCompleted := newQueueItem("old-completed", "webhook-1", "", testPayload, oldTime, oldTime)
	oldCompleted.Status = webhooks.QueueStatusCompleted
	oldPermanent := newQueueItem("old-permanent", "webhook-1", "", testPayload, oldTime, oldTime)
	oldPermanent.Status = webhooks.QueueStatusPermanentlyFailed
	newCompleted := newQueueItem("new-completed", "webhook-1", "", testPayload, now.Add(time.Hour), now.Add(time.Hour))
	newCompleted.Status = webhooks.QueueStatusCompleted
	for _, item := range []webhooks.QueueItem{oldCompleted, oldPermanent, newCompleted} {
		if err := fixture.repo.Enqueue(context.Background(), item); err != nil {
			t.Fatalf("enqueue cleanup item %s: %v", item.ID, err)
		}
	}

	worker := startWorker(t, fixture.service)
	waitForCount(t, fixture, "old-completed", 0)
	waitForCount(t, fixture, "old-permanent", 0)
	if got := countQueueItems(t, fixture.db, "new-completed"); got != 1 {
		t.Fatalf("new terminal item count = %d, want 1", got)
	}
	worker.stop(t)
}

func TestRetryDelayIsExponential(t *testing.T) {
	base := 7 * time.Millisecond
	for retryCount, want := range []time.Duration{base, 2 * base, 4 * base, 8 * base} {
		if got := webhooks.RetryDelayForTest(base, retryCount); got != want {
			t.Errorf("retry delay for count %d = %s, want %s", retryCount, got, want)
		}
	}
}
