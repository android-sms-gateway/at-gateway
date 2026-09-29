package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/config"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"go.uber.org/fx"
)

func TestDefaultMessages(t *testing.T) {
	cfg := config.Default()

	if got := cfg.Messages.PollInterval; got != time.Second {
		t.Errorf("Default().Messages.PollInterval = %v, want %v", got, time.Second)
	}
	if got := cfg.Messages.DefaultRegion; got != "RU" {
		t.Errorf("Default().Messages.DefaultRegion = %q, want %q", got, "RU")
	}
}

func TestNewOverridesMessagesFromEnv(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("MESSAGES__POLL_INTERVAL", "2s")
	t.Setenv("MESSAGES__DEFAULT_REGION", "DE")

	cfg, err := config.New()
	if err != nil {
		t.Fatalf("config.New() error = %v", err)
	}

	if got := cfg.Messages.PollInterval; got != 2*time.Second {
		t.Errorf("MESSAGES__POLL_INTERVAL mapped to %v, want %v", got, 2*time.Second)
	}
	if got := cfg.Messages.DefaultRegion; got != "DE" {
		t.Errorf("MESSAGES__DEFAULT_REGION mapped to %q, want %q", got, "DE")
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

func TestDefaultWebhooks(t *testing.T) {
	cfg := config.Default()

	if got := cfg.Webhooks.RetryCount; got != 15 {
		t.Errorf("Default().Webhooks.RetryCount = %d, want 15", got)
	}
	if got := cfg.Webhooks.SigningKey; got != "" {
		t.Errorf("Default().Webhooks.SigningKey = %q, want empty", got)
	}
	if got := cfg.Webhooks.Queue.BatchSize; got != 20 {
		t.Errorf("Default().Webhooks.Queue.BatchSize = %d, want 20", got)
	}

	queue := cfg.Webhooks.Queue
	durations := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{name: "request timeout", got: queue.RequestTimeout, want: 30 * time.Second},
		{name: "dial timeout", got: queue.DialTimeout, want: 5 * time.Second},
		{name: "retry base delay", got: queue.RetryBaseDelay, want: 5 * time.Second},
		{name: "idle delay", got: queue.IdleDelay, want: 5 * time.Second},
		{name: "stuck processing timeout", got: queue.StuckProcessingTimeout, want: 5 * time.Minute},
		{name: "cleanup retention", got: queue.CleanupRetention, want: 7 * 24 * time.Hour},
	}
	for _, tt := range durations {
		if tt.got != tt.want {
			t.Errorf("Default().Webhooks.Queue.%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

func resolveWebhooksConfig(t *testing.T) webhooks.Config {
	t.Helper()

	var got webhooks.Config
	app := fx.New(
		fx.NopLogger,
		config.Module(),
		fx.Invoke(func(cfg webhooks.Config) {
			got = cfg
		}),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("resolve webhooks config: %v", err)
	}
	return got
}

func TestNewWebhooksEnvAndFXBinding(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("WEBHOOKS__RETRY_COUNT", "7")
	t.Setenv("WEBHOOKS__SIGNING_KEY", "abc12345")
	t.Setenv("WEBHOOKS__QUEUE__BATCH_SIZE", "5")
	t.Setenv("WEBHOOKS__QUEUE__REQUEST_TIMEOUT", "31s")
	t.Setenv("WEBHOOKS__QUEUE__DIAL_TIMEOUT", "6s")
	t.Setenv("WEBHOOKS__QUEUE__RETRY_BASE_DELAY", "2s")
	t.Setenv("WEBHOOKS__QUEUE__IDLE_DELAY", "250ms")
	t.Setenv("WEBHOOKS__QUEUE__STUCK_PROCESSING_TIMEOUT", "10m")
	t.Setenv("WEBHOOKS__QUEUE__CLEANUP_RETENTION", "168h")

	got := resolveWebhooksConfig(t)

	if got.RetryCount != 7 {
		t.Errorf("RetryCount = %d, want 7", got.RetryCount)
	}
	if got.SigningKey != "abc12345" {
		t.Errorf("SigningKey = %q, want %q", got.SigningKey, "abc12345")
	}
	queue := got.Queue
	if queue.BatchSize != 5 {
		t.Errorf("Queue.BatchSize = %d, want 5", queue.BatchSize)
	}

	durations := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{name: "request timeout", got: queue.RequestTimeout, want: 31 * time.Second},
		{name: "dial timeout", got: queue.DialTimeout, want: 6 * time.Second},
		{name: "retry base delay", got: queue.RetryBaseDelay, want: 2 * time.Second},
		{name: "idle delay", got: queue.IdleDelay, want: 250 * time.Millisecond},
		{name: "stuck processing timeout", got: queue.StuckProcessingTimeout, want: 10 * time.Minute},
		{name: "cleanup retention", got: queue.CleanupRetention, want: 7 * 24 * time.Hour},
	}
	for _, tt := range durations {
		if tt.got != tt.want {
			t.Errorf("Queue.%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

func TestNewWebhooksEnvZeroValues(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("WEBHOOKS__RETRY_COUNT", "0")
	t.Setenv("WEBHOOKS__SIGNING_KEY", "")
	t.Setenv("WEBHOOKS__QUEUE__BATCH_SIZE", "0")
	t.Setenv("WEBHOOKS__QUEUE__REQUEST_TIMEOUT", "0s")
	t.Setenv("WEBHOOKS__QUEUE__DIAL_TIMEOUT", "0s")
	t.Setenv("WEBHOOKS__QUEUE__RETRY_BASE_DELAY", "0s")
	t.Setenv("WEBHOOKS__QUEUE__IDLE_DELAY", "0s")
	t.Setenv("WEBHOOKS__QUEUE__STUCK_PROCESSING_TIMEOUT", "0s")
	t.Setenv("WEBHOOKS__QUEUE__CLEANUP_RETENTION", "0s")

	got := resolveWebhooksConfig(t)
	if got.RetryCount != 0 || got.SigningKey != "" || got.Queue.BatchSize != 0 {
		t.Errorf("zero scalar config = %+v, want zero values", got)
	}
	if got.Queue.RequestTimeout != 0 ||
		got.Queue.DialTimeout != 0 ||
		got.Queue.RetryBaseDelay != 0 ||
		got.Queue.IdleDelay != 0 ||
		got.Queue.StuckProcessingTimeout != 0 ||
		got.Queue.CleanupRetention != 0 {
		t.Errorf("zero duration config = %+v, want zero values", got.Queue)
	}
}

func TestNewWebhooksRejectsInvalidDuration(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("WEBHOOKS__QUEUE__REQUEST_TIMEOUT", "not-a-duration")

	_, err := config.New()
	if err == nil {
		t.Fatal("config.New() error = nil, want invalid duration error")
	}
	if !strings.Contains(err.Error(), "webhooks.queue.request_timeout") {
		t.Errorf("config.New() error = %q, want field path", err)
	}
}
