package modem

import (
	"context"
	"fmt"
	"sync"

	"github.com/android-sms-gateway/at-gateway/internal/modem/at"
	"github.com/android-sms-gateway/at-gateway/internal/modem/port"
	"go.uber.org/zap"
)

type Service struct {
	config Config

	state    State
	info     Info
	sim      SimInfo
	commands *Commands

	port port.Port
	at   *at.AT

	mu sync.RWMutex

	logger  *zap.Logger
	metrics *Metrics
}

func NewService(config Config, loger *zap.Logger, metrics *Metrics) *Service {
	return &Service{
		config: config,

		state: StateDisconnected,
		info: Info{
			Manufacturer: "",
			Model:        "",
			IMEI:         "",
		},
		sim: SimInfo{
			PhoneNumber:       "",
			ICCID:             "",
			Carrier:           "",
			NetworkRegistered: false,
			SignalQuality:     0,
			SignalPercent:     0,
		},
		commands: nil,

		port: nil,
		at:   nil,

		mu: sync.RWMutex{},

		logger:  loger,
		metrics: metrics,
	}
}

func (s *Service) Run(ctx context.Context) error {
	s.logger.Info("connecting modem",
		zap.String("port", s.config.Port),
		zap.Int("baud", s.config.BaudRate),
	)

	if err := s.connect(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	info := s.Info()
	sim := s.SIM()
	s.logger.Info("modem ready",
		zap.String("manufacturer", info.Manufacturer),
		zap.String("model", info.Model),
		zap.String("imei", info.IMEI),
		zap.String("sim_iccid", sim.ICCID),
		zap.String("carrier", sim.Carrier),
		zap.Int("signal_percent", sim.SignalPercent),
	)

	s.mu.RLock()
	at := s.at
	s.mu.RUnlock()

	if at == nil {
		s.disconnect()
		return fmt.Errorf("modem not started: %w", ErrModemNotStarted)
	}

	select {
	case <-ctx.Done():
	case <-at.Done():
		s.logger.Warn("modem read loop exited unexpectedly")
	}

	s.disconnect()
	return nil
}

func (s *Service) connect(ctx context.Context) error {
	s.setState(StateConnecting)

	port, err := port.Open(port.Config{
		Name:     s.config.Port,
		BaudRate: s.config.BaudRate,
	})
	if err != nil {
		s.setState(StateError)
		return fmt.Errorf("open port %s: %w", s.config.Port, err)
	}

	at := at.NewAT(at.Config{Timeout: s.config.CommandTimeout}, port)
	at.Start()

	commands := NewCommands(at, CommandsConfig{
		CommandTimeout: s.config.CommandTimeout,
	})

	s.mu.Lock()
	s.port = port
	s.at = at
	s.commands = commands
	s.mu.Unlock()

	initCtx, cancel := context.WithTimeout(ctx, s.config.InitTimeout)
	defer cancel()

	if initErr := commands.Init(initCtx); initErr != nil {
		s.disconnect()
		s.setState(StateError)
		return fmt.Errorf("init: %w", initErr)
	}

	info, err := commands.GetModemInfo(ctx)
	if err != nil {
		s.logger.Warn("failed to get modem info", zap.Error(err))
	}

	sim, err := commands.GetSimInfo(ctx)
	if err != nil {
		s.logger.Warn("failed to get SIM info", zap.Error(err))
	}

	s.mu.Lock()
	s.info = info
	s.sim = sim
	s.mu.Unlock()

	s.metrics.SignalQuality.Set(float64(sim.SignalPercent))
	s.setState(StateReady)
	return nil
}

func (s *Service) disconnect() {
	s.mu.Lock()
	at := s.at
	port := s.port
	s.at = nil
	s.port = nil
	s.commands = nil
	s.state = StateDisconnected
	s.metrics.ModemState.Set(float64(StateDisconnected))
	s.mu.Unlock()

	if at != nil {
		at.Stop()
	}
	if port != nil {
		_ = port.Close()
	}
}

func (s *Service) setState(st State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = st
	s.metrics.ModemState.Set(float64(st))
}

func (s *Service) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *Service) Info() Info {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info
}

func (s *Service) SIM() SimInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sim
}

func (s *Service) ExecAT(ctx context.Context, cmd string) (*at.Response, error) {
	s.mu.RLock()
	at := s.at
	s.mu.RUnlock()

	if at == nil {
		return nil, ErrModemNotReady
	}

	res, err := at.Exec(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cmd, err)
	}

	return res, nil
}

func (s *Service) SignalUpdate(ctx context.Context) {
	s.mu.RLock()
	commands := s.commands
	s.mu.RUnlock()

	if commands == nil {
		return
	}

	sim, err := commands.GetSimInfo(ctx)
	if err != nil {
		s.logger.Debug("signal update failed", zap.Error(err))
		return
	}

	s.mu.Lock()
	if s.commands != commands {
		s.mu.Unlock()
		return
	}
	s.sim.SignalQuality = sim.SignalQuality
	s.sim.SignalPercent = sim.SignalPercent
	s.metrics.SignalQuality.Set(float64(sim.SignalPercent))
	s.mu.Unlock()
}
