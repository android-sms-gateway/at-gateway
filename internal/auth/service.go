package auth

import (
	"crypto/subtle"
	"fmt"

	"github.com/android-sms-gateway/at-gateway/internal/storage"
	gonanoid "github.com/matoous/go-nanoid/v2"
	"go.uber.org/zap"
)

const (
	DefaultUsername       = "sms"
	DefaultPasswordLength = 8

	keyPrefix   = "auth"
	keyUsername = keyPrefix + ".username"
	keyPassword = keyPrefix + ".password"
)

type Service struct {
	config Config

	storageSvc *storage.Service

	logger *zap.Logger
}

func NewService(config Config, storageSvc *storage.Service, logger *zap.Logger) (*Service, error) {
	return &Service{
		config: config,

		storageSvc: storageSvc,

		logger: logger,
	}, nil
}

func (s *Service) ValidateBasic(username, password string) error {
	if subtle.ConstantTimeCompare([]byte(username), []byte(s.resolveUsername())) == 0 {
		return ErrInvalidCredentials
	}

	passwordHash, err := s.resolvePassword()
	if err != nil {
		return err
	}

	if subtle.ConstantTimeCompare([]byte(password), []byte(passwordHash)) == 0 {
		return ErrInvalidCredentials
	}

	return nil
}

func (s *Service) resolveUsername() string {
	if username := s.config.Basic.Username; username != "" {
		return username
	}

	if username := s.storageSvc.Get(keyUsername); username != "" {
		return username
	}

	return DefaultUsername
}

func (s *Service) resolvePassword() (string, error) {
	password, _, err := s.ensureCredentials()

	return password, err
}

// ensureCredentials resolves the effective password with the following
// precedence: configured, stored, newly generated and persisted. generated
// reports that a fresh credential was created on first run.
func (s *Service) ensureCredentials() (string, bool, error) {
	if password := s.config.Basic.Password; password != "" {
		return password, false, nil
	}

	if password := s.storageSvc.Get(keyPassword); password != "" {
		return password, false, nil
	}

	password := gonanoid.Must(DefaultPasswordLength)
	if err := s.storageSvc.Set(keyPassword, password); err != nil {
		return "", false, fmt.Errorf("failed to persist generated password: %w", err)
	}

	return password, true, nil
}

// logBootstrapCredentials exposes first-run generated credentials so they are
// not silently discarded.
func (s *Service) logBootstrapCredentials() error {
	password, generated, err := s.ensureCredentials()
	if err != nil || !generated {
		return err
	}

	s.logger.Info(
		"generated initial credentials, set AUTH_BASIC_PASSWORD to use custom ones",
		zap.String("username", s.resolveUsername()),
		zap.String("password", password),
	)

	return nil
}
