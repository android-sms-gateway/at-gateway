package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"sync"

	"go.uber.org/zap"
)

type Service struct {
	config Config

	values map[string]string

	mu sync.RWMutex

	logger *zap.Logger
}

func NewService(config Config, loger *zap.Logger) (*Service, error) {
	if config.Path == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			config.Path = "data/storage.json"
		} else {
			config.Path = path.Join(configDir, "sms-gate", "at-gateway", "storage.json")
		}
	}

	if mkErr := os.MkdirAll(path.Dir(config.Path), 0700); mkErr != nil {
		return nil, fmt.Errorf("failed to create directory: %w", mkErr)
	}

	return &Service{
		config: config,

		values: map[string]string{},

		mu: sync.RWMutex{},

		logger: loger,
	}, nil
}

func (s *Service) Open() error {
	file, err := os.OpenFile(s.config.Path, os.O_RDONLY, 0600)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}

	defer file.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	if jsonErr := json.NewDecoder(file).Decode(&s.values); jsonErr != nil {
		return fmt.Errorf("failed to decode file: %w", jsonErr)
	}

	s.logger.Debug("storage opened", zap.String("path", s.config.Path), zap.Int("count", len(s.values)))

	return nil
}

func (s *Service) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value

	oldValue, existed := s.values[key]
	s.values[key] = value

	if err := s.saveLocked(); err != nil {
		if existed {
			s.values[key] = oldValue
		} else {
			delete(s.values, key)
		}
		return err
	}
	return nil
}

func (s *Service) SetMulti(values map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	maps.Copy(s.values, values)

	if err := s.saveLocked(); err != nil {
		s.logger.Error("failed to save storage", zap.Error(err))
	}
}

func (s *Service) Get(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.values[key]
}

func (s *Service) saveLocked() error {
	file, err := os.OpenFile(s.config.Path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	if jsonErr := json.NewEncoder(file).Encode(s.values); jsonErr != nil {
		return fmt.Errorf("failed to encode file: %w", jsonErr)
	}

	return nil
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.saveLocked(); err != nil {
		return err
	}

	s.logger.Debug("storage closed", zap.String("path", s.config.Path), zap.Int("count", len(s.values)))

	return nil
}
