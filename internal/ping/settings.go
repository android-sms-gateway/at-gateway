package ping

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/settings"
)

// keyIntervalSeconds is the ping settings key inside the ping scope: the row
// lands as "ping.interval_seconds". It is also the wire field name of the
// section (the json tag below): the scoped store's prefix is the wire
// section name, so storage path and wire path coincide by construction.
const keyIntervalSeconds = "interval_seconds"

// Settings owns the ping module's stored settings behind its prefix-scoped
// store: parsing, defaults, validation and unset semantics live here, never
// in the store.
type Settings struct {
	store  settings.Store
	config Config
}

// wireSection is the ping module's wire shape for BOTH directions of the
// settings API: Values marshals it, Update and Replace decode it.
type wireSection struct {
	IntervalSeconds *int `json:"interval_seconds,omitempty"`
}

// NewSettings returns the ping settings facade over its scoped store and the
// configured default.
func NewSettings(store settings.Store, config Config) *Settings {
	return &Settings{
		store:  store,
		config: config,
	}
}

// Interval returns the effective ping period: the stored whole-seconds value
// when set, otherwise the configured default. A NON-POSITIVE result is the
// documented ticker opt-out, and a malformed stored value is reported as an
// error rather than silently replaced by the default.
func (s *Settings) Interval(ctx context.Context) (time.Duration, error) {
	value, ok, err := s.store.Get(ctx, keyIntervalSeconds)
	if err != nil {
		return 0, fmt.Errorf("get ping interval: %w", err)
	}
	if !ok {
		return s.config.Interval, nil
	}

	seconds, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse stored ping interval %q: %w", value, err)
	}

	return time.Duration(seconds) * time.Second, nil
}

// Section returns the wire section name of the ping settings.
func (s *Settings) Section() string {
	return "ping"
}

// Values returns the effective section view as wire JSON: the stored period
// (whole seconds) else the configured default, clamped at zero so a
// disabled (non-positive) period reads back as 0.
func (s *Settings) Values(ctx context.Context) (json.RawMessage, error) {
	interval, err := s.Interval(ctx)
	if err != nil {
		return nil, err
	}

	seconds := max(0, int(interval.Seconds()))

	raw, err := json.Marshal(wireSection{IntervalSeconds: &seconds})
	if err != nil {
		return nil, fmt.Errorf("encode ping settings: %w", err)
	}

	return raw, nil
}

// Update applies a present section (PATCH semantics): an omitted field
// leaves the stored value untouched. A value below one is rejected with
// [settings.ErrInvalidValue] - the field's rule lives with its owner.
func (s *Settings) Update(ctx context.Context, section json.RawMessage) error {
	in, err := decode(section)
	if err != nil {
		return fmt.Errorf("update ping settings: %w", err)
	}
	if in.IntervalSeconds == nil {
		return nil
	}
	if *in.IntervalSeconds <= 0 {
		return fmt.Errorf(
			"%w: ping.%s must be at least 1",
			settings.ErrInvalidValue, keyIntervalSeconds,
		)
	}

	return s.setInterval(ctx, in.IntervalSeconds)
}

// Replace overwrites the section (PUT semantics): a nil or empty input
// clears the stored period so the configured default resumes.
func (s *Settings) Replace(ctx context.Context, section json.RawMessage) error {
	in, err := decode(section)
	if err != nil {
		return fmt.Errorf("replace ping settings: %w", err)
	}
	if in.IntervalSeconds != nil && *in.IntervalSeconds <= 0 {
		return fmt.Errorf(
			"%w: ping.%s must be at least 1",
			settings.ErrInvalidValue, keyIntervalSeconds,
		)
	}

	return s.setInterval(ctx, in.IntervalSeconds)
}

// setInterval stores seconds, or unsets the stored value when seconds is nil
// so the configured default resumes (PUT replacement semantics).
func (s *Settings) setInterval(ctx context.Context, seconds *int) error {
	if seconds == nil {
		if err := s.store.Delete(ctx, keyIntervalSeconds); err != nil {
			return fmt.Errorf("clear ping interval: %w", err)
		}

		return nil
	}

	if err := s.store.Set(ctx, keyIntervalSeconds, strconv.Itoa(*seconds)); err != nil {
		return fmt.Errorf("set ping interval: %w", err)
	}

	return nil
}

// decode unmarshals a section payload; an empty payload yields the zero
// shape (every field nil) so Replace can clear through it.
func decode(section json.RawMessage) (wireSection, error) {
	if len(section) == 0 {
		return wireSection{IntervalSeconds: nil}, nil
	}

	var in wireSection
	if err := json.Unmarshal(section, &in); err != nil {
		return wireSection{}, fmt.Errorf("decode ping settings: %w", err)
	}

	return in, nil
}
