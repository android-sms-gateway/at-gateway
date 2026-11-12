package ping

import (
	"context"
	"fmt"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"go.uber.org/zap"
)

// Service drives the periodic system:ping emission on its own lifecycle, so the
// ticker stops with the app instead of leaking.
//
// It only ENQUEUES: the queue delivery worker (webhooks.Service.Run) remains the
// single HTTP performer, so ticking can never perform a request itself and
// cannot interfere with delivery.
type Service struct {
	webhooksSvc *webhooks.Service
	settings    *Settings
	logger      *zap.Logger
}

// NewService returns the system:ping ticker driven by the module settings.
func NewService(settings *Settings, webhooksSvc *webhooks.Service, logger *zap.Logger) *Service {
	return &Service{
		webhooksSvc: webhooksSvc,
		settings:    settings,
		logger:      logger,
	}
}

// Run loads the stored interval once, at start - migrations have run by
// then, and a settings write requires a restart to be applied - then drives
// the ticker until ctx is canceled. A read failure aborts the app rather than
// ticking on a default the API already confirmed as replaced; a shutdown that
// interrupts the read stops cleanly with nil.
func (s *Service) Run(ctx context.Context) error {
	interval, err := s.settings.Interval(ctx)
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("load ping interval: %w", err)
	}
	if ctx.Err() == nil {
		return s.tick(ctx, interval)
	}

	return nil
}

// tick emits a system:ping event on every tick until ctx is canceled and then
// returns nil. A NON-POSITIVE interval is the documented opt-out: the ticker
// returns immediately and no event is ever emitted.
//
// The first tick happens one full interval after start, not at start, so a
// restart does not re-blast a heartbeat on top of the app:started event.
//
// The emission cadence is FIXED and independent of delivery outcomes: a slow or
// failing endpoint makes the queue grow at exactly one item per interval, it
// never accelerates the ticker. Retries belong to the queue worker, per item.
func (s *Service) tick(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		// Info, not Debug: the disabled ticker is otherwise invisible, and the
		// operator who set the opt-out needs to see it land. It fires once.
		s.logger.Info("system ping emission disabled", zap.Duration("ping_interval", interval))

		return nil
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.webhooksSvc.EmitSystemPing()
		}
	}
}
