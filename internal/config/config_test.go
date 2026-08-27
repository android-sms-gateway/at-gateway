package config_test

import (
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/config"
	"github.com/android-sms-gateway/at-gateway/internal/messages"
)

func TestDefaultMessages(t *testing.T) {
	cfg := config.Default()

	if got := cfg.Messages.PollInterval; got != time.Second {
		t.Errorf("Default().Messages.PollInterval = %v, want %v", got, time.Second)
	}
}

func TestMessagesDefaultsMatchPackage(t *testing.T) {
	cfg := config.Default()
	want := messages.Default()

	if cfg.Messages.PollInterval != want.PollInterval {
		t.Errorf(
			"raw PollInterval default = %v, want %v (must mirror messages.Default)",
			cfg.Messages.PollInterval,
			want.PollInterval,
		)
	}
}

func TestNewOverridesMessagesFromEnv(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("MESSAGES__POLL_INTERVAL", "2s")

	cfg, err := config.New()
	if err != nil {
		t.Fatalf("config.New() error = %v", err)
	}

	if got := cfg.Messages.PollInterval; got != 2*time.Second {
		t.Errorf("MESSAGES__POLL_INTERVAL mapped to %v, want %v", got, 2*time.Second)
	}
	if got := cfg.HTTP.Address; got != "127.0.0.1:3000" {
		t.Errorf("HTTP.Address = %q, want default %q (unrelated keys untouched)", got, "127.0.0.1:3000")
	}
}

func TestNewMessagesEnvVariations(t *testing.T) {
	tests := []struct {
		name         string
		pollInterval string
		wantPoll     time.Duration
	}{
		{
			name:         "short interval",
			pollInterval: "500ms",
			wantPoll:     500 * time.Millisecond,
		},
		{
			name:         "zero interval maps through",
			pollInterval: "0s",
			wantPoll:     0,
		},
		{
			name:         "empty device id overrides default",
			pollInterval: "3s",
			wantPoll:     3 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONFIG_PATH", "")
			t.Setenv("MESSAGES__POLL_INTERVAL", tt.pollInterval)

			cfg, err := config.New()
			if err != nil {
				t.Fatalf("config.New() error = %v", err)
			}

			if got := cfg.Messages.PollInterval; got != tt.wantPoll {
				t.Errorf("PollInterval = %v, want %v", got, tt.wantPoll)
			}
		})
	}
}
