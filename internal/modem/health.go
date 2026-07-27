package modem

import (
	"context"

	"github.com/go-core-fx/healthfx"
)

const healthProviderName = "modem"

type HealthProvider struct {
	svc *Service
}

func NewHealthProvider(svc *Service) *HealthProvider {
	return &HealthProvider{svc: svc}
}

func (p *HealthProvider) Name() string {
	return healthProviderName
}

func (p *HealthProvider) StartedProbe(_ context.Context) (healthfx.Checks, error) {
	return healthfx.Checks{}, nil
}

func (p *HealthProvider) ReadyProbe(_ context.Context) (healthfx.Checks, error) {
	state := p.svc.State()
	status := healthfx.StatusPass
	switch state {
	case StateReady:

	case StateDisconnected, StateConnecting:
		status = healthfx.StatusFail
	case StateBusy, StateError:
		status = healthfx.StatusWarn
	}

	return healthfx.Checks{
		healthProviderName: {
			Status:        status,
			Description:   "modem status: " + state.String(),
			ObservedValue: state,
			ObservedUnit:  "",
		},
	}, nil
}

func (p *HealthProvider) LiveProbe(_ context.Context) (healthfx.Checks, error) {
	return healthfx.Checks{}, nil
}
