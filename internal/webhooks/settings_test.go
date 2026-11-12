package webhooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/settings"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
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

	svc := webhooks.NewSettings(&memStore{}, webhooks.Config{RetryCount: 15})
	if got := svc.Section(); got != "webhooks" {
		t.Errorf("Section() = %q, want webhooks", got)
	}
}

// TestValues_OnlyRetryCountAndSigningKeyAbsent pins the write-only contract
// at the source: even with a stored signing key, the section view carries
// exactly the retry count - the key can never enter the aggregated response.
func TestValues_OnlyRetryCountAndSigningKeyAbsent(t *testing.T) {
	t.Parallel()

	store := &memStore{values: map[string]string{
		"retry_count": "7",
		"signing_key": "stored-secret",
	}}
	svc := webhooks.NewSettings(store, webhooks.Config{RetryCount: 15})

	raw, err := svc.Values(context.Background())
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	got := view(t, raw)
	if len(got) != 1 {
		t.Fatalf("view = %v, want exactly one wire field", got)
	}
	if got["retry_count"] != float64(7) {
		t.Errorf("retry_count = %v, want 7 (stored wins)", got["retry_count"])
	}
	if _, present := got["signing_key"]; present {
		t.Errorf("signing_key present in view %v, want write-only", got)
	}
}

// TestValues_ConfigDefaultWhenUnset pins the effective default: an unset
// retry count serves the configured value.
func TestValues_ConfigDefaultWhenUnset(t *testing.T) {
	t.Parallel()

	svc := webhooks.NewSettings(&memStore{}, webhooks.Config{RetryCount: 15})

	raw, err := svc.Values(context.Background())
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if got := view(t, raw)["retry_count"]; got != float64(15) {
		t.Errorf("retry_count = %v, want 15 (config default)", got)
	}
}

// TestSigningKey_EmptyStoredCountsAsUnset pins the fallback: an empty stored
// row can never shadow the configured key.
func TestSigningKey_EmptyStoredCountsAsUnset(t *testing.T) {
	t.Parallel()

	store := &memStore{values: map[string]string{"signing_key": ""}}
	svc := webhooks.NewSettings(store, webhooks.Config{SigningKey: "configured-secret"})

	got, err := svc.SigningKey(context.Background())
	if err != nil {
		t.Fatalf("SigningKey: %v", err)
	}
	if got != "configured-secret" {
		t.Errorf("SigningKey() = %q, want configured key (empty stored value treated as unset)", got)
	}
}

// TestUpdate_EmptySigningKeyClearsRow pins the write path through the
// section surface: writing an empty signing key unsets the row so the
// configured key resumes, and no empty row lingers behind.
func TestUpdate_EmptySigningKeyClearsRow(t *testing.T) {
	t.Parallel()

	store := &memStore{values: map[string]string{"signing_key": "stored-secret"}}
	svc := webhooks.NewSettings(store, webhooks.Config{SigningKey: "configured-secret"})

	if err := svc.Update(context.Background(), json.RawMessage(`{"signing_key":""}`)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, present := store.values["signing_key"]; present {
		t.Error("signing_key row present after empty write, want deleted")
	}

	got, err := svc.SigningKey(context.Background())
	if err != nil {
		t.Fatalf("SigningKey: %v", err)
	}
	if got != "configured-secret" {
		t.Errorf("SigningKey() = %q, want configured key after clear", got)
	}
}

// TestUpdate_AppliesProvidedFields pins PATCH semantics: a present signing
// key is stored, an omitted retry count keeps its row.
func TestUpdate_AppliesProvidedFields(t *testing.T) {
	t.Parallel()

	store := &memStore{values: map[string]string{"retry_count": "7"}}
	svc := webhooks.NewSettings(store, webhooks.Config{RetryCount: 15})

	if err := svc.Update(context.Background(), json.RawMessage(`{"signing_key":"k1"}`)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := store.values["signing_key"]; got != "k1" {
		t.Errorf("signing_key = %q, want k1", got)
	}
	if got := store.values["retry_count"]; got != "7" {
		t.Errorf("retry_count = %q, want 7 (untouched)", got)
	}
}

// TestUpdate_BelowMinimumRejected pins the section-owned field rule: a
// retry count below one surfaces as settings.ErrInvalidValue (the handler
// maps it to a wire 400) and writes nothing.
func TestUpdate_BelowMinimumRejected(t *testing.T) {
	t.Parallel()

	store := &memStore{}
	svc := webhooks.NewSettings(store, webhooks.Config{RetryCount: 15})

	err := svc.Update(context.Background(), json.RawMessage(`{"retry_count":0}`))
	if !errors.Is(err, settings.ErrInvalidValue) {
		t.Fatalf("Update error = %v, want ErrInvalidValue", err)
	}
	if _, written := store.values["retry_count"]; written {
		t.Errorf("row written on rejected value: %v", store.values)
	}
}

// TestReplace_NilClearsBothFields pins the PUT reset: an absent section
// clears retry count and signing key so the configured defaults resume.
func TestReplace_NilClearsBothFields(t *testing.T) {
	t.Parallel()

	store := &memStore{values: map[string]string{
		"retry_count": "7",
		"signing_key": "k1",
	}}
	svc := webhooks.NewSettings(store, webhooks.Config{RetryCount: 15})

	if err := svc.Replace(context.Background(), nil); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if _, present := store.values["retry_count"]; present {
		t.Errorf("retry_count survived empty Replace: %v", store.values)
	}
	if _, present := store.values["signing_key"]; present {
		t.Errorf("signing_key survived empty Replace: %v", store.values)
	}
}

// TestReplace_ProvidedFieldOverwrites pins the PUT write path: fields in the
// payload take the new value while omitted siblings still clear.
func TestReplace_ProvidedFieldOverwrites(t *testing.T) {
	t.Parallel()

	store := &memStore{values: map[string]string{
		"retry_count": "7",
		"signing_key": "k1",
	}}
	svc := webhooks.NewSettings(store, webhooks.Config{RetryCount: 15})

	if err := svc.Replace(context.Background(), json.RawMessage(`{"retry_count":3}`)); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := store.values["retry_count"]; got != "3" {
		t.Errorf("retry_count = %q, want 3", got)
	}
	if _, present := store.values["signing_key"]; present {
		t.Errorf("signing_key survived Replace without the field: %v", store.values)
	}
}

// TestValues_MalformedStoredErrors pins integrity handling: a stored value
// that is not an integer is reported, never silently replaced by default.
func TestValues_MalformedStoredErrors(t *testing.T) {
	t.Parallel()

	store := &memStore{values: map[string]string{"retry_count": "not-a-number"}}
	svc := webhooks.NewSettings(store, webhooks.Config{RetryCount: 15})

	if _, err := svc.Values(context.Background()); err == nil {
		t.Fatal("Values error = nil, want parse error")
	}
}
