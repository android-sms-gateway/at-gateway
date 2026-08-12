package auth_test

import (
	"path/filepath"
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/auth"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"go.uber.org/zap"
)

func newTestService(t *testing.T, cfg auth.Config) (*auth.Service, *storage.Service, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "storage.json")

	storageSvc, err := storage.NewService(storage.Config{Path: path}, zap.NewNop())
	if err != nil {
		t.Fatalf("failed to create storage service: %v", err)
	}

	svc, err := auth.NewService(cfg, storageSvc, zap.NewNop())
	if err != nil {
		t.Fatalf("failed to create auth service: %v", err)
	}

	return svc, storageSvc, path
}

func newReader(t *testing.T, path string) *storage.Service {
	t.Helper()

	reader, err := storage.NewService(storage.Config{Path: path}, zap.NewNop())
	if err != nil {
		t.Fatalf("failed to create reader storage service: %v", err)
	}
	if openErr := reader.Open(); openErr != nil {
		t.Fatalf("failed to open reader storage service: %v", openErr)
	}

	return reader
}

func TestResolvePassword_ConfiguredWins(t *testing.T) {
	svc, storageSvc, _ := newTestService(t, auth.Config{
		Basic: auth.BasicConfig{
			Username: "admin",
			Password: "configured",
		},
	})

	if err := svc.ValidateBasic("admin", "configured"); err != nil {
		t.Fatalf("expected configured credentials to pass, got %v", err)
	}

	if stored := storageSvc.Get("auth.password"); stored != "" {
		t.Fatalf("expected no persisted password, got %q", stored)
	}
}

func TestResolvePassword_StoredReused(t *testing.T) {
	svc, storageSvc, _ := newTestService(t, auth.Config{})
	if err := storageSvc.Set("auth.password", "stored"); err != nil {
		t.Fatalf("failed to store password: %v", err)
	}

	if err := svc.ValidateBasic(auth.DefaultUsername, "stored"); err != nil {
		t.Fatalf("expected stored credentials to pass, got %v", err)
	}
}

func TestResolvePassword_GeneratesOnFirstRun(t *testing.T) {
	svc, storageSvc, _ := newTestService(t, auth.Config{})

	if err := svc.ValidateBasic(auth.DefaultUsername, "wrong"); err == nil {
		t.Fatal("expected unknown password to fail before generation")
	}

	stored := storageSvc.Get("auth.password")
	if len(stored) != auth.DefaultPasswordLength {
		t.Fatalf("expected generated password of length %d, got %q", auth.DefaultPasswordLength, stored)
	}

	if err := svc.ValidateBasic(auth.DefaultUsername, stored); err != nil {
		t.Fatalf("expected generated credentials to pass, got %v", err)
	}
}

func TestResolvePassword_GeneratedPersisted(t *testing.T) {
	svc, storageSvc, path := newTestService(t, auth.Config{})

	_ = svc.ValidateBasic(auth.DefaultUsername, "wrong")
	password := storageSvc.Get("auth.password")

	reader := newReader(t, path)
	if persisted := reader.Get("auth.password"); persisted != password {
		t.Fatalf("expected generated password to be persisted, got %q", persisted)
	}
}
