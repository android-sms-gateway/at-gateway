package messages

import (
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"go.uber.org/zap"
)

// NewMessageModel exposes the message model mapper for external tests.
func NewMessageModel(msg *MessageInput, now time.Time) (*messageModel, error) {
	return newMessageModel(msg, now)
}

// NewRecipientModels exposes the recipient model mapper for external tests.
func NewRecipientModels(recipients []string, messageID int64, now time.Time) []recipientModel {
	return newRecipientModels(recipients, messageID, now)
}

// NewStatesModel builds a state history for external tests.
func NewStatesModel(entries ...StateChange) statesModel {
	model := make(statesModel, 0, len(entries))
	for _, entry := range entries {
		model = append(model, stateModel(entry))
	}

	return model
}

// ToDomain maps the message model onto the domain message.
func (m *messageModel) ToDomain() (*Message, error) {
	return m.toDomain()
}

// ToDomain maps a recipient model onto the domain recipient.
func (m *recipientModel) ToDomain() Recipient {
	return m.toDomain()
}

// ToDomainMap collapses the state history onto the first-occurrence map.
func (s statesModel) ToDomainMap() map[string]time.Time {
	return s.toDomainMap()
}

// DeriveMessageState exposes the android state ladder for external tests.
func DeriveMessageState(recipientStates []smsgateway.ProcessingState) smsgateway.ProcessingState {
	return deriveMessageState(recipientStates)
}

// NewServiceWithMetrics exposes the metrics-aware service constructor for
// external tests; the public NewService wires no metrics.
func NewServiceWithMetrics(
	config Config,
	messages *Repository,
	devicesSvc *devices.Service,
	modemSvc *modem.Service,
	metrics *Metrics,
	logger *zap.Logger,
) *Service {
	return NewService(config, messages, devicesSvc, modemSvc, metrics, logger)
}
