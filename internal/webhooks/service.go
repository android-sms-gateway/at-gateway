package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/client-go/smsgateway"
	gonanoid "github.com/matoous/go-nanoid/v2"
	"go.uber.org/zap"
)

// Service implements the webhook registry, queue emission and delivery worker:
// validation, ID generation, persistence and fire-and-forget event enqueueing.
type Service struct {
	config Config

	webhooks   *Repository
	devicesSvc *devices.Service
	storageSvc *storage.Service

	metrics *Metrics
	logger  *zap.Logger

	signingKey string
}

// NewService wires the registry and emission service with its configuration,
// repository, device authority, persistent key-value storage, metrics and logger.
func NewService(
	config Config,
	webhooks *Repository,
	devicesSvc *devices.Service,
	storageSvc *storage.Service,
	metrics *Metrics,
	logger *zap.Logger,
) (*Service, error) {
	return &Service{
		config:     config,
		webhooks:   webhooks,
		devicesSvc: devicesSvc,
		storageSvc: storageSvc,
		metrics:    metrics,
		logger:     logger,
		signingKey: "",
	}, nil
}

type eventEnvelope struct {
	ID        string                  `json:"id"`
	WebhookID string                  `json:"webhookId"`
	Event     smsgateway.WebhookEvent `json:"event"`
	DeviceID  string                  `json:"deviceId"`
	Payload   any                     `json:"payload"`
}

const (
	signingKeyStorageKey = "webhooks.signing_key"
	signingKeyLength     = 32
)

var (
	errWebhookHTTPStatus = errors.New("webhook returned non-success HTTP status")
)

// emit selects webhooks registered for event and enqueues one delivery item
// for each match. It performs database work only; HTTP delivery belongs to the
// T8 worker. All errors are logged and swallowed so message processing cannot
// be affected by webhook emission.
func (s *Service) emit(event smsgateway.WebhookEvent, payload any) {
	ctx := context.Background()
	items, err := s.webhooks.SelectByEvent(ctx, event)
	if err != nil {
		s.logger.Error("select webhooks for event", zap.Error(err), zap.String("event", event))
		return
	}
	if len(items) == 0 {
		return
	}

	deviceID := s.devicesSvc.Get().ID
	now := time.Now().UTC()
	for _, item := range items {
		enqueueErr := s.enqueue(ctx, item, event, payload, deviceID, now)
		if enqueueErr != nil {
			s.logger.Error(
				"enqueue webhook event",
				zap.Error(enqueueErr),
				zap.String("event", event),
				zap.String("webhook_id", item.ID),
			)
			continue
		}

		if s.metrics != nil {
			s.metrics.IncEnqueued()
		}
	}
}

func (s *Service) enqueue(
	ctx context.Context,
	webhook Webhook,
	event smsgateway.WebhookEvent,
	payload any,
	deviceID string,
	now time.Time,
) error {
	envelope := eventEnvelope{
		ID:        gonanoid.Must(),
		WebhookID: webhook.ID,
		Event:     event,
		DeviceID:  deviceID,
		Payload:   payload,
	}

	encoded, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal webhook envelope: %w", err)
	}

	item := QueueItem{
		ID:          gonanoid.Must(),
		WebhookID:   webhook.ID,
		URL:         webhook.URL,
		Payload:     string(encoded),
		RetryCount:  0,
		Status:      QueueStatusPending,
		CreatedAt:   now,
		NextAttempt: now,
		LastError:   nil,
	}
	enqueueErr := s.webhooks.Enqueue(ctx, item)
	if enqueueErr != nil {
		return fmt.Errorf("enqueue webhook queue item: %w", enqueueErr)
	}

	return nil
}

// Run is the single webhook queue delivery goroutine. It recovers stale
// claims, processes due items sequentially and returns when ctx is canceled.
func (s *Service) Run(ctx context.Context) error {
	s.signingKey = s.config.SigningKey
	if s.signingKey == "" {
		// storage.Get is value-only, so a read failure is indistinguishable from
		// an absent value; fall through to generation and surface any Set failure.
		s.signingKey = s.storageSvc.Get(signingKeyStorageKey)
		if s.signingKey == "" {
			s.signingKey = gonanoid.Must(signingKeyLength)
			if err := s.storageSvc.Set(signingKeyStorageKey, s.signingKey); err != nil {
				return fmt.Errorf("resolve webhook signing key: %w", err)
			}
		}
	}

	client := newWebhookHTTPClient(s.config.Queue)

	for ctx.Err() == nil {
		if !s.processQueuePass(ctx, client) {
			return nil
		}
	}
	return nil
}

func (s *Service) processQueuePass(ctx context.Context, client *http.Client) bool {
	now := time.Now().UTC()
	if recoverErr := s.webhooks.RecoverStuck(ctx, now.Add(-s.config.Queue.StuckProcessingTimeout)); recoverErr != nil {
		s.logger.Error("recover stuck webhook queue items", zap.Error(recoverErr))
	}

	items, err := s.webhooks.DueItems(ctx, now, s.config.Queue.BatchSize)
	if err != nil {
		s.logger.Error("select due webhook queue items", zap.Error(err))
		return waitForQueuePoll(ctx, s.config.Queue.IdleDelay)
	}

	if len(items) == 0 {
		if cleanupErr := s.webhooks.Cleanup(ctx, now.Add(-s.config.Queue.CleanupRetention)); cleanupErr != nil {
			s.logger.Error("cleanup webhook queue items", zap.Error(cleanupErr))
		}
		return waitForQueuePoll(ctx, s.config.Queue.IdleDelay)
	}

	for _, item := range items {
		if ctx.Err() != nil {
			return false
		}
		if markErr := s.webhooks.MarkProcessing(ctx, item.ID); markErr != nil {
			s.logger.Error("mark webhook queue item processing", zap.Error(markErr), zap.String("queue_id", item.ID))
			continue
		}

		if deliveryErr := s.deliver(ctx, client, item); deliveryErr != nil {
			s.markDeliveryFailure(ctx, item, deliveryErr)
			continue
		}
		if completeErr := s.webhooks.MarkCompleted(ctx, item.ID); completeErr != nil {
			s.logger.Error("mark webhook queue item completed", zap.Error(completeErr), zap.String("queue_id", item.ID))
			continue
		}
		s.metrics.IncDelivered()
	}
	return true
}

func newWebhookHTTPClient(queue QueueConfig) *http.Client {
	dialer := new(net.Dialer)
	dialer.Timeout = queue.DialTimeout

	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: queue.DialTimeout,
			// Android's socket timeout is approximated here; Queue exposes
			// one dial/TLS timeout, so it also bounds response headers.
			ResponseHeaderTimeout: queue.DialTimeout,
		},
		Timeout: queue.RequestTimeout,
	}
}

func (s *Service) deliver(ctx context.Context, client *http.Client, item QueueItem) error {
	timestamp := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, item.URL, strings.NewReader(item.Payload))
	if err != nil {
		return fmt.Errorf("create webhook request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Timestamp", timestamp)
	request.Header.Set("X-Signature", signPayload(s.signingKey, item.Payload, timestamp))

	response, err := client.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				s.logger.Debug("close webhook response body", zap.Error(closeErr))
			}
		}
		return fmt.Errorf("post webhook: %w", err)
	}
	if response.Body != nil {
		defer func() {
			if closeErr := response.Body.Close(); closeErr != nil {
				s.logger.Debug("close webhook response body", zap.Error(closeErr))
			}
		}()
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%w: %d (%s)", errWebhookHTTPStatus, response.StatusCode, response.Status)
	}

	return nil
}

func signPayload(key, body, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(body))
	_, _ = mac.Write([]byte(timestamp))
	return hex.EncodeToString(mac.Sum(nil))
}

func retryDelay(base time.Duration, retryCount int) time.Duration {
	if retryCount < 0 {
		return 0
	}
	return base << retryCount
}

func (s *Service) markDeliveryFailure(ctx context.Context, item QueueItem, deliveryErr error) {
	if s.metrics != nil {
		s.metrics.IncFailed()
	}

	if item.RetryCount < s.config.RetryCount && item.Status != QueueStatusPermanentlyFailed {
		nextAttempt := time.Now().UTC().Add(retryDelay(s.config.Queue.RetryBaseDelay, item.RetryCount))
		if err := s.webhooks.MarkFailed(ctx, item.ID, nextAttempt, deliveryErr.Error()); err != nil {
			s.logger.Error("mark webhook queue item failed", zap.Error(err), zap.String("queue_id", item.ID))
		}
		return
	}

	if err := s.webhooks.MarkPermanentlyFailed(ctx, item.ID, deliveryErr.Error()); err != nil {
		s.logger.Error("mark webhook queue item permanently failed", zap.Error(err), zap.String("queue_id", item.ID))
		return
	}
	if s.metrics != nil {
		s.metrics.IncPermanentlyFailed()
	}
}

func waitForQueuePoll(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Select returns all registered webhooks; a stored SQL NULL device_id maps
// to DeviceID nil (registered for all devices). An empty registry yields an
// empty non-nil slice.
func (s *Service) Select(ctx context.Context) ([]smsgateway.Webhook, error) {
	items, err := s.webhooks.Select(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to select webhooks: %w", err)
	}

	result := make([]smsgateway.Webhook, 0, len(items))
	for _, item := range items {
		result = append(result, smsgateway.Webhook{
			ID:       item.ID,
			DeviceID: item.DeviceID,
			URL:      item.URL,
			Event:    item.Event,
		})
	}

	return result, nil
}

// Replace validates the webhook, resolves the device scope, generates the id
// when empty (mutating webhook for server 201-echo parity) and upserts it.
// Device scope: nil or empty deviceId registers for ALL devices (stored as
// SQL NULL, dto normalized to nil for the 201 echo - the local device id is
// NEVER substituted); a supplied deviceId must equal the local device id or
// ErrDeviceNotFound is returned. It does not enforce HTTPS itself: the edge
// validator calls client-go Webhook.Validate(), which rejects invalid event
// types and URLs that do not start with https:// before this runs.
func (s *Service) Replace(ctx context.Context, webhook *smsgateway.Webhook) error {
	if !smsgateway.IsValidWebhookEvent(webhook.Event) {
		return fmt.Errorf("%w: %q", ErrInvalidEvent, webhook.Event)
	}

	if webhook.DeviceID == nil || *webhook.DeviceID == "" {
		webhook.DeviceID = nil
	} else if *webhook.DeviceID != s.devicesSvc.Get().ID {
		return fmt.Errorf("%w: %q", ErrDeviceNotFound, *webhook.DeviceID)
	}

	if webhook.ID == "" {
		webhook.ID = gonanoid.Must()
	}

	if err := s.webhooks.Replace(ctx, Webhook{
		ID:       webhook.ID,
		DeviceID: webhook.DeviceID,
		URL:      webhook.URL,
		Event:    webhook.Event,
	}); err != nil {
		return fmt.Errorf("failed to replace webhook: %w", err)
	}

	return nil
}

// Delete removes the webhook with the given id; unknown ids are a no-op, so
// delete is idempotent (server parity: no not-found error).
func (s *Service) Delete(ctx context.Context, id string) error {
	if err := s.webhooks.Delete(ctx, id); err != nil {
		return fmt.Errorf("failed to delete webhook: %w", err)
	}

	return nil
}
