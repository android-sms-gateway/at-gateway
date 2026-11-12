package ping_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/ping"
	"github.com/android-sms-gateway/at-gateway/internal/settings"
)

// memStore is an in-memory settings.Store fixture: the facade under test
// only needs Get/Set/Delete semantics, so no persistence graph is involved.
type memStore struct {
	values map[string]string
}

func (m *memStore) Get(_ context.Context, key string) (string, bool, error) {
	value, ok := m.values[key]

	return value, ok, nil
}

func (m *memStore) Set(_ context.Context, key, value string) error {
	if m.values == nil {
		m.values = make(map[string]string)
	}
	m.values[key] = value

	return nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	delete(m.values, key)

	return nil
}

var _ settings.Store = (*memStore)(nil)

// view decodes a Values payload for assertions.
func view(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode section view %q: %v", raw, err)
	}

	return out
}

// TestSection_Name pins the wire section name used by the aggregator.
func TestSection_Name(t *testing.T) {
	t.Parallel()

	svc := ping.NewSettings(&memStore{}, ping.Config{Interval: time.Second})
	if got := svc.Section(); got != "ping" {
		t.Errorf("Section() = %q, want ping", got)
	}
}

// TestValues_ConfigDefaultClamped pins the wire view of the configured
// default: whole seconds, and a disabled (non-positive) period reads back as
// 0 - the clamp lives here, in the module that owns the semantics.
func TestValues_ConfigDefaultClamped(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		interval time.Duration
		want     float64
	}{
		{name: "enabled default", interval: 60 * time.Second, want: 60},
		{name: "sub-second truncates", interval: 1500 * time.Millisecond, want: 1},
		{name: "disabled clamps to zero", interval: -5 * time.Second, want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := ping.NewSettings(&memStore{}, ping.Config{Interval: tt.interval})
			raw, err := svc.Values(context.Background())
			if err != nil {
				t.Fatalf("Values: %v", err)
			}
			got := view(t, raw)
			if len(got) != 1 || got["interval_seconds"] != tt.want {
				t.Errorf("view = %v, want {interval_seconds: %v}", got, tt.want)
			}
		})
	}
}

// TestValues_StoredOverridesConfig pins the effective read: a stored row
// wins over the configured default under the same wire field name.
func TestValues_StoredOverridesConfig(t *testing.T) {
	t.Parallel()

	store := &memStore{values: map[string]string{"interval_seconds": "45"}}
	svc := ping.NewSettings(store, ping.Config{Interval: 60 * time.Second})

	raw, err := svc.Values(context.Background())
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if got := view(t, raw)["interval_seconds"]; got != float64(45) {
		t.Errorf("interval_seconds = %v, want 45 (stored wins)", got)
	}
}

// TestValues_MalformedStoredErrors pins integrity handling: a stored value
// that is not an integer is reported, never silently replaced by default.
func TestValues_MalformedStoredErrors(t *testing.T) {
	t.Parallel()

	store := &memStore{values: map[string]string{"interval_seconds": "not-a-number"}}
	svc := ping.NewSettings(store, ping.Config{Interval: 60 * time.Second})

	if _, err := svc.Values(context.Background()); err == nil {
		t.Fatal("Values error = nil, want parse error")
	}
}

// TestUpdate_AppliesProvidedField pins PATCH semantics: a present field is
// stored under its wire/storage key.
func TestUpdate_AppliesProvidedField(t *testing.T) {
	t.Parallel()

	store := &memStore{}
	svc := ping.NewSettings(store, ping.Config{Interval: 60 * time.Second})

	if err := svc.Update(context.Background(), json.RawMessage(`{"interval_seconds":45}`)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := store.values["interval_seconds"]; got != "45" {
		t.Errorf("stored = %q, want 45", got)
	}
}

// TestUpdate_OmittedFieldKeepsStoredValue pins the PATCH no-overwrite rule:
// a present section without the field leaves the row untouched.
func TestUpdate_OmittedFieldKeepsStoredValue(t *testing.T) {
	t.Parallel()

	store := &memStore{values: map[string]string{"interval_seconds": "45"}}
	svc := ping.NewSettings(store, ping.Config{Interval: 60 * time.Second})

	if err := svc.Update(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := store.values["interval_seconds"]; got != "45" {
		t.Errorf("stored = %q, want 45 (untouched)", got)
	}
}

// TestUpdate_BelowMinimumRejected pins the section-owned field rule: a value
// below one surfaces as settings.ErrInvalidValue (the handler maps it to a
// wire 400) and writes nothing.
func TestUpdate_BelowMinimumRejected(t *testing.T) {
	t.Parallel()

	store := &memStore{}
	svc := ping.NewSettings(store, ping.Config{Interval: 60 * time.Second})

	err := svc.Update(context.Background(), json.RawMessage(`{"interval_seconds":0}`))
	if !errors.Is(err, settings.ErrInvalidValue) {
		t.Fatalf("Update error = %v, want ErrInvalidValue", err)
	}
	if _, written := store.values["interval_seconds"]; written {
		t.Errorf("row written on rejected value: %v", store.values)
	}
}

// TestReplace_NilClearsStoredValue pins the PUT reset: an absent section
// clears the row so the configured default resumes.
func TestReplace_NilClearsStoredValue(t *testing.T) {
	t.Parallel()

	store := &memStore{values: map[string]string{"interval_seconds": "45"}}
	svc := ping.NewSettings(store, ping.Config{Interval: 60 * time.Second})

	if err := svc.Replace(context.Background(), nil); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if _, present := store.values["interval_seconds"]; present {
		t.Errorf("row survived empty Replace: %v", store.values)
	}
}

// TestReplace_ProvidedFieldOverwrites pins the PUT write path.
func TestReplace_ProvidedFieldOverwrites(t *testing.T) {
	t.Parallel()

	store := &memStore{values: map[string]string{"interval_seconds": "45"}}
	svc := ping.NewSettings(store, ping.Config{Interval: 60 * time.Second})

	if err := svc.Replace(context.Background(), json.RawMessage(`{"interval_seconds":30}`)); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := store.values["interval_seconds"]; got != "30" {
		t.Errorf("stored = %q, want 30", got)
	}
}
