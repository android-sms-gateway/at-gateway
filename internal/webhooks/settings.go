package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/android-sms-gateway/at-gateway/internal/settings"
)

// Settings keys inside the webhooks scope: the rows land as
// "webhooks.retry_count" and "webhooks.signing_key". Each key is also the
// wire field name of the section (the json tags below): the scoped store's
// prefix is the wire section name, so storage path and wire path coincide by
// construction.
const (
	keyRetryCount = "retry_count"
	keySigningKey = "signing_key"
)

// Settings owns the webhook module's stored settings behind its
// prefix-scoped store: parsing, defaults, validation and unset semantics
// live here, never in the store. The signing key stored here only ever comes
// from the settings API; runtime generation stays with the delivery worker.
type Settings struct {
	store  settings.Store
	config Config
}

// wireSection is the webhook module's wire shape for BOTH directions of the
// settings API: Values marshals it (SigningKey never set - write-only),
// Update and Replace decode it.
type wireSection struct {
	RetryCount *int    `json:"retry_count,omitempty"`
	SigningKey *string `json:"signing_key,omitempty"`
}

// NewSettings returns the webhook settings facade over its scoped store and
// the configured defaults.
func NewSettings(store settings.Store, config Config) *Settings {
	return &Settings{
		store:  store,
		config: config,
	}
}

// RetryCount returns the effective delivery attempt limit: the stored value
// when set, otherwise the configured default. A malformed stored value is
// reported as an error rather than silently replaced by the default.
func (s *Settings) RetryCount(ctx context.Context) (int, error) {
	value, ok, err := s.store.Get(ctx, keyRetryCount)
	if err != nil {
		return 0, fmt.Errorf("get webhook retry count: %w", err)
	}
	if !ok {
		return s.config.RetryCount, nil
	}

	count, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse stored webhook retry count %q: %w", value, err)
	}

	return count, nil
}

// Section returns the wire section name of the webhook settings.
func (s *Settings) Section() string {
	return "webhooks"
}

// Values returns the effective section view as wire JSON: exactly the retry
// count. The signing key is deliberately absent - it is write-only on the
// API surface, so the aggregated view can never include it.
func (s *Settings) Values(ctx context.Context) (json.RawMessage, error) {
	count, err := s.RetryCount(ctx)
	if err != nil {
		return nil, err
	}

	raw, err := json.Marshal(wireSection{RetryCount: &count, SigningKey: nil})
	if err != nil {
		return nil, fmt.Errorf("encode webhook settings: %w", err)
	}

	return raw, nil
}

// Update applies a present section (PATCH semantics): omitted fields leave
// their stored values untouched. A retry count below one is rejected with
// [settings.ErrInvalidValue]; an empty signing key clears the stored row
// (treated as unset).
func (s *Settings) Update(ctx context.Context, section json.RawMessage) error {
	in, err := decode(section)
	if err != nil {
		return fmt.Errorf("update webhook settings: %w", err)
	}
	if in.RetryCount != nil {
		if *in.RetryCount <= 0 {
			return fmt.Errorf(
				"%w: webhooks.%s must be at least 1",
				settings.ErrInvalidValue, keyRetryCount,
			)
		}
		if err = s.setRetryCount(ctx, in.RetryCount); err != nil {
			return err
		}
	}
	if in.SigningKey != nil {
		return s.setSigningKey(ctx, in.SigningKey)
	}

	return nil
}

// Replace overwrites the section (PUT semantics): a nil or empty input
// clears both fields so the configured defaults resume.
func (s *Settings) Replace(ctx context.Context, section json.RawMessage) error {
	in, err := decode(section)
	if err != nil {
		return fmt.Errorf("replace webhook settings: %w", err)
	}
	if in.RetryCount != nil && *in.RetryCount <= 0 {
		return fmt.Errorf(
			"%w: webhooks.%s must be at least 1",
			settings.ErrInvalidValue, keyRetryCount,
		)
	}
	if err = s.setRetryCount(ctx, in.RetryCount); err != nil {
		return err
	}

	return s.setSigningKey(ctx, in.SigningKey)
}

// SigningKey returns the signing key the worker should start from: the
// stored key when non-empty, otherwise the configured key, otherwise ""
// (the worker falls back to its persisted storage key, then generates one).
// An empty stored value counts as unset so it can never shadow the
// configured key. It is never exposed through the API.
func (s *Settings) SigningKey(ctx context.Context) (string, error) {
	value, ok, err := s.store.Get(ctx, keySigningKey)
	if err != nil {
		return "", fmt.Errorf("get webhook signing key: %w", err)
	}
	if ok && value != "" {
		return value, nil
	}

	return s.config.SigningKey, nil
}

// setRetryCount stores count, or unsets the stored value when count is nil
// so the configured default resumes (PUT replacement semantics).
func (s *Settings) setRetryCount(ctx context.Context, count *int) error {
	if count == nil {
		if err := s.store.Delete(ctx, keyRetryCount); err != nil {
			return fmt.Errorf("clear webhook retry count: %w", err)
		}

		return nil
	}

	if err := s.store.Set(ctx, keyRetryCount, strconv.Itoa(*count)); err != nil {
		return fmt.Errorf("set webhook retry count: %w", err)
	}

	return nil
}

// setSigningKey stores key, or unsets the stored value when key is nil or
// empty so the configured key resumes (PUT replacement semantics; an empty
// string is treated as unset).
func (s *Settings) setSigningKey(ctx context.Context, key *string) error {
	if key == nil || *key == "" {
		if err := s.store.Delete(ctx, keySigningKey); err != nil {
			return fmt.Errorf("clear webhook signing key: %w", err)
		}

		return nil
	}

	if err := s.store.Set(ctx, keySigningKey, *key); err != nil {
		return fmt.Errorf("set webhook signing key: %w", err)
	}

	return nil
}

// decode unmarshals a section payload; an empty payload yields the zero
// shape (every field nil) so Replace can clear through it.
func decode(section json.RawMessage) (wireSection, error) {
	if len(section) == 0 {
		return wireSection{RetryCount: nil, SigningKey: nil}, nil
	}

	var in wireSection
	if err := json.Unmarshal(section, &in); err != nil {
		return wireSection{}, fmt.Errorf("decode webhook settings: %w", err)
	}

	return in, nil
}
