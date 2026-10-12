package config_test

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/config"
	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"go.uber.org/fx"
)

// resolveMessagesConfig and resolveModemConfig resolve the per-domain configs
// through the fx binding layer, i.e. exactly what the running app receives.
func resolveMessagesConfig(t *testing.T) messages.Config {
	t.Helper()

	var got messages.Config
	app := fx.New(
		fx.NopLogger,
		config.Module(),
		fx.Invoke(func(cfg messages.Config) {
			got = cfg
		}),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("resolve messages config: %v", err)
	}

	return got
}

func resolveModemConfig(t *testing.T) modem.Config {
	t.Helper()

	var got modem.Config
	app := fx.New(
		fx.NopLogger,
		config.Module(),
		fx.Invoke(func(cfg modem.Config) {
			got = cfg
		}),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("resolve modem config: %v", err)
	}

	return got
}

// TestDefaultInboundAndCmgd pins the built-in defaults of the knobs the inbound
// SMS path depends on.
func TestDefaultInboundAndCmgd(t *testing.T) {
	cfg := config.Default()

	if got := cfg.Messages.Inbound.ReassemblyTimeout; got != 5*time.Minute {
		t.Errorf("Default().Messages.Inbound.ReassemblyTimeout = %v, want 5m", got)
	}
	if got := cfg.Modem.Cmgd.RetryAttempts; got != 3 {
		t.Errorf("Default().Modem.Cmgd.RetryAttempts = %d, want 3", got)
	}
	if got := cfg.Modem.Cmgd.RetryBackoff; got != 2*time.Second {
		t.Errorf("Default().Modem.Cmgd.RetryBackoff = %v, want 2s", got)
	}
	// The 30s command timeout is what makes the retry arithmetic meaningful.
	if got := cfg.Modem.CommandTimeout; got != 30*time.Second {
		t.Errorf("Default().Modem.CommandTimeout = %v, want 30s", got)
	}
	if got := cfg.Modem.Store.Memory; got != "SM" {
		t.Errorf("Default().Modem.Store.Memory = %q, want SM", got)
	}
	if got := cfg.Modem.Store.SweepInterval; got != 60*time.Second {
		t.Errorf("Default().Modem.Store.SweepInterval = %v, want 60s", got)
	}
}

// TestNewInboundAndCmgdEnvOverridesAndFXBinding pins that the new knobs are
// BOUND in the module (not merely declared) and can be overridden per key,
// including the zero values that disable the reassembly window and the retry
// loop.
func TestNewInboundAndCmgdEnvOverridesAndFXBinding(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("MESSAGES__INBOUND__REASSEMBLY_TIMEOUT", "90s")
	t.Setenv("MODEM__CMGD__RETRY_ATTEMPTS", "5")
	t.Setenv("MODEM__CMGD__RETRY_BACKOFF", "250ms")
	t.Setenv("MODEM__STORE__MEMORY", "ME")
	t.Setenv("MODEM__STORE__SWEEP_INTERVAL", "15s")

	messagesCfg := resolveMessagesConfig(t)
	if got := messagesCfg.Inbound.ReassemblyTimeout; got != 90*time.Second {
		t.Errorf("MESSAGES__INBOUND__REASSEMBLY_TIMEOUT bound to %v, want 90s", got)
	}
	if got := messagesCfg.PollInterval; got != time.Second {
		t.Errorf("Messages.PollInterval = %v, want the untouched default 1s", got)
	}

	modemCfg := resolveModemConfig(t)
	if got := modemCfg.Cmgd.RetryAttempts; got != 5 {
		t.Errorf("MODEM__CMGD__RETRY_ATTEMPTS bound to %d, want 5", got)
	}
	if got := modemCfg.Cmgd.RetryBackoff; got != 250*time.Millisecond {
		t.Errorf("MODEM__CMGD__RETRY_BACKOFF bound to %v, want 250ms", got)
	}
	if got := modemCfg.CommandTimeout; got != 30*time.Second {
		t.Errorf("Modem.CommandTimeout = %v, want the untouched default 30s", got)
	}
	if got := modemCfg.Store.Memory; got != "ME" {
		t.Errorf("MODEM__STORE__MEMORY bound to %q, want ME", got)
	}
	if got := modemCfg.Store.SweepInterval; got != 15*time.Second {
		t.Errorf("MODEM__STORE__SWEEP_INTERVAL bound to %v, want 15s", got)
	}
}

// TestNewInboundAndCmgdZeroValues pins the zero boundary: a disabled reassembly
// window and a single CMGD attempt map through unchanged (the messages service
// then applies its own non-positive fallback).
func TestNewInboundAndCmgdZeroValues(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("MESSAGES__INBOUND__REASSEMBLY_TIMEOUT", "0s")
	t.Setenv("MODEM__CMGD__RETRY_ATTEMPTS", "0")
	t.Setenv("MODEM__CMGD__RETRY_BACKOFF", "0s")

	messagesCfg := resolveMessagesConfig(t)
	if got := messagesCfg.Inbound.ReassemblyTimeout; got != 0 {
		t.Errorf("Messages.Inbound.ReassemblyTimeout = %v, want 0", got)
	}

	modemCfg := resolveModemConfig(t)
	if got := modemCfg.Cmgd; got != (modem.CmgdConfig{RetryAttempts: 0, RetryBackoff: 0}) {
		t.Errorf("Modem.Cmgd = %+v, want zero values", got)
	}
}

// TestNewRejectsInvalidInboundDuration pins the validation surface of the new
// duration keys.
func TestNewRejectsInvalidInboundDuration(t *testing.T) {
	tests := []struct {
		key   string
		field string
	}{
		{key: "MESSAGES__INBOUND__REASSEMBLY_TIMEOUT", field: "messages.inbound.reassembly_timeout"},
		{key: "MODEM__CMGD__RETRY_BACKOFF", field: "modem.cmgd.retry_backoff"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Setenv("CONFIG_PATH", "")
			t.Setenv(tt.key, "not-a-duration")

			_, err := config.New()
			if err == nil {
				t.Fatalf("config.New() error = nil, want invalid duration error for %s", tt.key)
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Errorf("config.New() error = %q, want the field path %q", err, tt.field)
			}
		})
	}
}

// TestEnvExampleDocumentsNewKeysWithDefaults pins the operator surface: every
// new key is present in .env.example with a value EQUAL to the built-in
// default, and its comment block documents that same default - so the
// documentation cannot drift from the code.
func TestEnvExampleDocumentsNewKeysWithDefaults(t *testing.T) {
	raw, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	cfg := config.Default()
	tests := []struct {
		key string
		// documented is the value (and documented default) .env.example must
		// carry.
		documented string
		// wantDuration and wantAttempts pin that the documented value EQUALS the
		// built-in default - documentation drift fails the test.
		wantDuration time.Duration
		wantAttempts int
	}{
		{
			key:          "MESSAGES__INBOUND__REASSEMBLY_TIMEOUT",
			documented:   "5m",
			wantDuration: cfg.Messages.Inbound.ReassemblyTimeout,
			wantAttempts: -1,
		},
		{
			key:          "MODEM__CMGD__RETRY_BACKOFF",
			documented:   "2s",
			wantDuration: cfg.Modem.Cmgd.RetryBackoff,
			wantAttempts: -1,
		},
		{
			key:          "MODEM__CMGD__RETRY_ATTEMPTS",
			documented:   "3",
			wantDuration: -1,
			wantAttempts: cfg.Modem.Cmgd.RetryAttempts,
		},
	}

	lines := strings.Split(string(raw), "\n")
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			index := lineIndex(t, lines, tt.key+"=")
			if got := strings.TrimSpace(strings.TrimPrefix(lines[index], tt.key+"=")); got != tt.documented {
				t.Errorf("%s = %q in .env.example, want %q", tt.key, got, tt.documented)
			}

			comment := commentBlockAbove(lines, index)
			if !strings.Contains(comment, "default: "+tt.documented) {
				t.Errorf(
					"%s comment block does not document (default: %s):\n%s",
					tt.key,
					tt.documented,
					comment,
				)
			}

			if tt.wantAttempts >= 0 {
				parsed, parseErr := strconv.Atoi(tt.documented)
				if parseErr != nil || parsed != tt.wantAttempts {
					t.Errorf(
						"documented default %q does not match the built-in default %d",
						tt.documented,
						tt.wantAttempts,
					)
				}

				return
			}

			parsed, parseErr := time.ParseDuration(tt.documented)
			if parseErr != nil || parsed != tt.wantDuration {
				t.Errorf(
					"documented default %q does not match the built-in default %v",
					tt.documented,
					tt.wantDuration,
				)
			}
		})
	}
}

// TestEnvExampleDocumentsStoreKeys pins the operator surface of the preferred
// store settings: both keys exist in .env.example with values EQUAL to the
// built-in defaults and are documented in their comment block, so the
// documentation cannot drift from the code.
func TestEnvExampleDocumentsStoreKeys(t *testing.T) {
	raw, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	lines := strings.Split(string(raw), "\n")

	memIndex := lineIndex(t, lines, "MODEM__STORE__MEMORY=")
	if got := strings.TrimSpace(
		strings.TrimPrefix(lines[memIndex], "MODEM__STORE__MEMORY="),
	); got != config.Default().Modem.Store.Memory {
		t.Errorf("MODEM__STORE__MEMORY = %q in .env.example, want %q", got, config.Default().Modem.Store.Memory)
	}

	const sweepKey = "MODEM__STORE__SWEEP_INTERVAL"
	// The documented spelling is pinned literally (60s, not 1m); the assertion
	// below compares it to the built-in default SEMANTICALLY.
	const documented = "60s"
	sweepIndex := lineIndex(t, lines, sweepKey+"=")
	if got := strings.TrimSpace(strings.TrimPrefix(lines[sweepIndex], sweepKey+"=")); got != documented {
		t.Errorf("%s = %q in .env.example, want %q", sweepKey, got, documented)
	}
	if comment := commentBlockAbove(lines, sweepIndex); !strings.Contains(comment, "default: "+documented) {
		t.Errorf("%s comment block does not document (default: %s):\n%s", sweepKey, documented, comment)
	}

	parsed, err := time.ParseDuration(documented)
	if err != nil {
		t.Fatalf("parse documented default %q: %v", documented, err)
	}
	if want := config.Default().Modem.Store.SweepInterval; parsed != want {
		t.Errorf("documented default %q = %v, want the built-in default %v", documented, parsed, want)
	}
}

// lineIndex returns the index of the first line with the given prefix.
func lineIndex(t *testing.T, lines []string, prefix string) int {
	t.Helper()

	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return i
		}
	}
	t.Fatalf("key %q is missing from .env.example", prefix)

	return 0
}

// commentBlockAbove returns the contiguous comment lines directly above index.
func commentBlockAbove(lines []string, index int) string {
	block := make([]string, 0, 4)
	for i := index - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "#") {
			break
		}
		block = append([]string{trimmed}, block...)
	}

	return strings.Join(block, "\n")
}
