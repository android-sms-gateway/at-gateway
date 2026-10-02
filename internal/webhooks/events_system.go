package webhooks

import (
	"context"
	"time"

	"github.com/android-sms-gateway/client-go/smsgateway"
	"go.uber.org/zap"
)

// EmitSystemPing emits a system:ping event: the periodic heartbeat a consumer
// uses to tell a live gateway from a dead one.
//
// FIRE-AND-FORGET, like every non-inbound emitter: emit() swallows every error
// and returns no verdict, because the caller has no delivery obligation to
// report - see the tracked/fire-and-forget split documented in
// events_received.go. With no webhook registered for the event the tick is a
// no-op: emit() selects zero matches and performs no queue write.
//
// PAYLOAD PARITY GAP (accepted, deliberate): the wire payload is the EMPTY JSON
// OBJECT {} because the client-go contract type is
// smsgateway.SystemPingPayload{} (dto_webhooks.go:136), which carries no field.
// The Android gateway publishes {"health": {...}} on the same event
// (EventsReceiver.kt:21-29), so an Android consumer of system:ping cannot read
// a health document from at-gateway. Introducing one would require a client-go
// release chain and would change a published wire type; neither is in scope, so
// the divergence is documented instead.
func (s *Service) EmitSystemPing() {
	s.emit(
		smsgateway.WebhookEventSystemPing,
		smsgateway.SystemPingPayload{},
	)
}

// PingEmitter drives the periodic system:ping emission on its own lifecycle,
// so the ticker stops with the app instead of leaking.
//
// It only ENQUEUES: the queue delivery worker (Service.Run) remains the single
// HTTP performer in this package, so ticking can never perform a request
// itself and cannot interfere with delivery.
type PingEmitter struct {
	service  *Service
	interval time.Duration
	logger   *zap.Logger
}

// NewPingEmitter returns the system:ping ticker for the configured period.
func NewPingEmitter(config Config, service *Service, logger *zap.Logger) *PingEmitter {
	return &PingEmitter{
		service:  service,
		interval: config.PingInterval,
		logger:   logger,
	}
}

// Run emits a system:ping event on every tick until ctx is canceled and then
// returns nil. A NON-POSITIVE interval is the documented opt-out: the ticker
// returns immediately and no event is ever emitted.
//
// The first tick happens one full interval after start, not at start, so a
// restart does not re-blast a heartbeat on top of the app:started event.
//
// The emission cadence is FIXED and independent of delivery outcomes: a slow or
// failing endpoint makes the queue grow at exactly one item per interval, it
// never accelerates the ticker. Retries belong to the queue worker, per item.
func (p *PingEmitter) Run(ctx context.Context) error {
	if p.interval <= 0 {
		// Info, not Debug: the disabled ticker is otherwise invisible, and the
		// operator who set the opt-out needs to see it land. It fires once.
		p.logger.Info("system ping emission disabled", zap.Duration("ping_interval", p.interval))

		return nil
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.service.EmitSystemPing()
		}
	}
}
