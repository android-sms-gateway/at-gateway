package devices

import (
	"os"
	"sync"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/storage"
	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/samber/lo"
	"go.uber.org/zap"
)

const (
	keyPrefix    = "device"
	keyID        = keyPrefix + ".id"
	keyName      = keyPrefix + ".name"
	keyCreatedAt = keyPrefix + ".created_at"
)

type Service struct {
	config Config

	storageSvc *storage.Service

	mu sync.Mutex

	logger *zap.Logger
}

func NewService(config Config, storageSvc *storage.Service, logger *zap.Logger) *Service {
	return &Service{
		config: config,

		storageSvc: storageSvc,

		mu: sync.Mutex{},

		logger: logger,
	}
}

func (s *Service) Get() Device {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := s.storageSvc.Get(keyID)
	name := s.storageSvc.Get(keyName)
	createdAt := s.storageSvc.Get(keyCreatedAt)

	if id != "" && createdAt != "" {
		return Device{
			ID:        id,
			Name:      name,
			CreatedAt: lo.Must(time.Parse(time.RFC3339, createdAt)),
		}
	}

	id = gonanoid.Must()
	name = s.config.Name
	if name == "" {
		name, _ = lo.TryOr(os.Hostname, "unknown-"+id[:8])
	}
	now := time.Now()
	createdAt = now.Format(time.RFC3339)

	s.storageSvc.SetMulti(
		map[string]string{
			keyID:        id,
			keyName:      name,
			keyCreatedAt: createdAt,
		},
	)

	return Device{
		ID:        id,
		Name:      name,
		CreatedAt: now,
	}
}
