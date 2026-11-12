package settings

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/android-sms-gateway/client-go/smsgateway"
)

// Service aggregates the module settings sections behind the settings API.
// Its input and output are both smsgateway.DeviceSettings; the only
// interchange format it works with in between is per-section raw JSON, so
// the settings module knows no per-module fields. Storage stays key-value
// behind the sections.
type Service struct {
	sections []Section
}

// NewService returns the settings aggregator over its grouped sections.
func NewService(sections []Section) *Service {
	return &Service{sections: sections}
}

// Get returns the effective settings as the wire DTO: every section
// contributes its raw view into one nested object (wire section -> wire
// fields), empty contributions are skipped, and the JSON round-trip into
// smsgateway.DeviceSettings drops anything the DTO does not model - so
// webhooks.signing_key and un-honored sections are structurally never
// included.
func (s *Service) Get(ctx context.Context) (smsgateway.DeviceSettings, error) {
	merged := make(map[string]json.RawMessage, len(s.sections))
	for _, section := range s.sections {
		raw, err := section.Values(ctx)
		if err != nil {
			return smsgateway.DeviceSettings{}, fmt.Errorf("read %s settings: %w", section.Section(), err)
		}
		if len(raw) == 0 {
			continue
		}
		merged[section.Section()] = raw
	}

	raw, err := json.Marshal(merged)
	if err != nil {
		return smsgateway.DeviceSettings{}, fmt.Errorf("marshal settings view: %w", err)
	}

	var out smsgateway.DeviceSettings
	if err = json.Unmarshal(raw, &out); err != nil {
		return smsgateway.DeviceSettings{}, fmt.Errorf("decode settings view: %w", err)
	}

	return out, nil
}

// Update applies the provided sections (PATCH semantics): sections
// and fields absent from in leave their stored values untouched. It returns
// the effective view after the writes, identical to GetSettings.
func (s *Service) Update(ctx context.Context, in smsgateway.DeviceSettings) (smsgateway.DeviceSettings, error) {
	input, err := split(in)
	if err != nil {
		return smsgateway.DeviceSettings{}, fmt.Errorf("split settings input: %w", err)
	}

	for _, section := range s.sections {
		raw, ok := input[section.Section()]
		if !ok {
			continue
		}
		if err = section.Update(ctx, raw); err != nil {
			return smsgateway.DeviceSettings{}, fmt.Errorf("update %s settings: %w", section.Section(), err)
		}
	}

	return s.Get(ctx)
}

// Replace overwrites every section (PUT semantics): sections absent
// from in are passed as nil so their fields clear and the configured
// defaults resume. It returns the effective view after the writes, identical
// to GetSettings.
func (s *Service) Replace(
	ctx context.Context,
	in smsgateway.DeviceSettings,
) (smsgateway.DeviceSettings, error) {
	input, err := split(in)
	if err != nil {
		return smsgateway.DeviceSettings{}, fmt.Errorf("split settings input: %w", err)
	}

	for _, section := range s.sections {
		if err = section.Replace(ctx, input[section.Section()]); err != nil {
			return smsgateway.DeviceSettings{}, fmt.Errorf("replace %s settings: %w", section.Section(), err)
		}
	}

	return s.Get(ctx)
}

// split serializes the device settings and breaks the result into
// per-section raw JSON; omitted sections are simply absent from the map.
func split(in smsgateway.DeviceSettings) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal device settings: %w", err)
	}

	var sections map[string]json.RawMessage
	if err = json.Unmarshal(raw, &sections); err != nil {
		return nil, fmt.Errorf("split device settings: %w", err)
	}

	return sections, nil
}
