package webhooks

import (
	"time"

	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// WebhookModel aliases the unexported persisted model so the external
// webhooks_test package can exercise the model converters directly
// (file exempt from the testpackage linter).
type WebhookModel = webhookModel

// NewWebhookModel exposes newWebhookModel to external tests.
func NewWebhookModel(w Webhook, now time.Time) WebhookModel {
	return newWebhookModel(w, now)
}

// ToDomain exposes webhookModel.toDomain to external tests.
func (m WebhookModel) ToDomain() Webhook {
	return m.toDomain()
}

// QueueItemModel aliases the unexported queue model for external tests.
type QueueItemModel = queueItemModel

// NewQueueItemModel exposes newQueueItemModel to external tests.
func NewQueueItemModel(item QueueItem) QueueItemModel {
	return newQueueItemModel(item)
}

// ToDomain exposes queueItemModel.toDomain to external tests.
func (m QueueItemModel) ToDomain() QueueItem {
	return m.toDomain()
}

// NewTestMetrics exposes the isolated metrics constructor to external tests.
func NewTestMetrics(registerer prometheus.Registerer) *Metrics {
	return newMetrics(registerer)
}

// CounterValue reads a named webhook counter for external tests.
func CounterValue(m *Metrics, name string) float64 {
	switch name {
	case "at_gateway_webhooks_enqueued_total":
		return testutil.ToFloat64(m.enqueued)
	case "at_gateway_webhooks_delivered_total":
		return testutil.ToFloat64(m.delivered)
	case "at_gateway_webhooks_failed_total":
		return testutil.ToFloat64(m.failed)
	case "at_gateway_webhooks_permanently_failed_total":
		return testutil.ToFloat64(m.permanentlyFailed)
	default:
		panic("unknown webhook counter: " + name)
	}
}

// SigningKey exposes the constructor-resolved signing key to external tests.
func (s *Service) SigningKey() string {
	return s.signingKey
}

// SignPayloadForTest exposes the deterministic signer to external tests.
func SignPayloadForTest(key, body, timestamp string) string {
	return signPayload(key, body, timestamp)
}

// EmitForTest exposes the unexported generic emission path for the error-path test.
func (s *Service) EmitForTest(event smsgateway.WebhookEvent, payload any) {
	s.emit(event, payload)
}

// RetryDelayForTest exposes the exponential retry delay to external tests.
func RetryDelayForTest(base time.Duration, retryCount int) time.Duration {
	return retryDelay(base, retryCount)
}
