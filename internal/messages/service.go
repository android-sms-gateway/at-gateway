package messages

import (
	"context"

	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"go.uber.org/zap"
)

type Service struct {
	config   Config
	messages *Repository

	devicesSvc *devices.Service
	modemSvc   *modem.Service

	logger *zap.Logger
}

func NewService(
	config Config,
	messages *Repository,
	devicesSvc *devices.Service,
	modemSvc *modem.Service,
	logger *zap.Logger,
) *Service {
	return &Service{
		config:   config,
		messages: messages,

		devicesSvc: devicesSvc,
		modemSvc:   modemSvc,

		logger: logger,
	}
}

func (s *Service) Run(ctx context.Context) error {
	return nil
}

func (s *Service) Enqueue(ctx context.Context, message MessageInput) (*Message, error) {
	panic("unimplemented")
}

func (s *Service) List(ctx context.Context, filter ListFilter) ([]Message, int, error) {
	panic("unimplemented")
}

func (s *Service) Get(ctx context.Context, id string) (*Message, error) {
	panic("unimplemented")
}

func (s *Service) Cancel(ctx context.Context, id string) (*Message, error) {
	panic("unimplemented")
}
