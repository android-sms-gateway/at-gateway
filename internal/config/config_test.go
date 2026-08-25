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
	if got := cfg.Messages.DeviceID; got != "default" {
		t.Errorf("Default().Messages.DeviceID = %q, want %q", got, "default")
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
	if cfg.Messages.DeviceID != want.DeviceID {
		t.Errorf(
			"raw DeviceID default = %q, want %q (must mirror messages.Default)",
			cfg.Messages.DeviceID,
			want.DeviceID,
		)
	}
}

func TestNewOverridesMessagesFromEnv(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("MESSAGES__POLL_INTERVAL", "2s")
	t.Setenv("MESSAGES__DEVICE_ID", "sim-2")

	cfg, err := config.New()
	if err != nil {
		t.Fatalf("config.New() error = %v", err)
	}

	if got := cfg.Messages.PollInterval; got != 2*time.Second {
		t.Errorf("MESSAGES__POLL_INTERVAL mapped to %v, want %v", got, 2*time.Second)
	}
	if got := cfg.Messages.DeviceID; got != "sim-2" {
		t.Errorf("MESSAGES__DEVICE_ID mapped to %q, want %q", got, "sim-2")
	}
	if got := cfg.HTTP.Address; got != "127.0.0.1:3000" {
		t.Errorf("HTTP.Address = %q, want default %q (unrelated keys untouched)", got, "127.0.0.1:3000")
	}
}

func TestNewMessagesEnvVariations(t *testing.T) {
	tests := []struct {
		name         string
		pollInterval string
		deviceID     string
		wantPoll     time.Duration
		wantDevice   string
	}{
		{
			name:         "short interval",
			pollInterval: "500ms",
			deviceID:     "sim-a",
			wantPoll:     500 * time.Millisecond,
			wantDevice:   "sim-a",
		},
		{
			name:         "zero interval maps through",
			pollInterval: "0s",
			deviceID:     "device-42",
			wantPoll:     0,
			wantDevice:   "device-42",
		},
		{
			name:         "empty device id overrides default",
			pollInterval: "3s",
			deviceID:     "",
			wantPoll:     3 * time.Second,
			wantDevice:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONFIG_PATH", "")
			t.Setenv("MESSAGES__POLL_INTERVAL", tt.pollInterval)
			t.Setenv("MESSAGES__DEVICE_ID", tt.deviceID)

			cfg, err := config.New()
			if err != nil {
				t.Fatalf("config.New() error = %v", err)
			}

			if got := cfg.Messages.PollInterval; got != tt.wantPoll {
				t.Errorf("PollInterval = %v, want %v", got, tt.wantPoll)
			}
			if got := cfg.Messages.DeviceID; got != tt.wantDevice {
				t.Errorf("DeviceID = %q, want %q", got, tt.wantDevice)
			}
		})
	}
}
