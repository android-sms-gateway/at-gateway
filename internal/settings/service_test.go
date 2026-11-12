package settings_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/settings"
	"github.com/android-sms-gateway/client-go/smsgateway"
)

// stubPingSection mirrors ping.Settings over raw wire JSON: state in whole
// seconds, recorded Update/Replace payloads.
type stubPingSection struct {
	interval int

	valuesErr  error
	updateErr  error
	replaceErr error

	updateCalls  []json.RawMessage
	replaceCalls []json.RawMessage
}

func (s *stubPingSection) Section() string { return "ping" }

func (s *stubPingSection) Values(context.Context) (json.RawMessage, error) {
	if s.valuesErr != nil {
		return nil, s.valuesErr
	}

	return json.Marshal(struct {
		IntervalSeconds int `json:"interval_seconds"`
	}{IntervalSeconds: s.interval})
}

func (s *stubPingSection) Update(_ context.Context, section json.RawMessage) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updateCalls = append(s.updateCalls, section)

	var in struct {
		IntervalSeconds *int `json:"interval_seconds"`
	}
	if err := json.Unmarshal(section, &in); err != nil {
		return err
	}
	if in.IntervalSeconds != nil {
		s.interval = *in.IntervalSeconds
	}

	return nil
}

func (s *stubPingSection) Replace(_ context.Context, section json.RawMessage) error {
	if s.replaceErr != nil {
		return s.replaceErr
	}
	s.replaceCalls = append(s.replaceCalls, section)

	s.interval = 0
	if len(section) == 0 {
		return nil
	}

	var in struct {
		IntervalSeconds *int `json:"interval_seconds"`
	}
	if err := json.Unmarshal(section, &in); err != nil {
		return err
	}
	if in.IntervalSeconds != nil {
		s.interval = *in.IntervalSeconds
	}

	return nil
}

// stubWebhooksSection mirrors webhooks.Settings over raw wire JSON; the
// signing key state is not modeled because it never appears on the read
// side - its delivery is asserted on the recorded payloads.
type stubWebhooksSection struct {
	retryCount int

	valuesErr  error
	updateErr  error
	replaceErr error

	updateCalls  []json.RawMessage
	replaceCalls []json.RawMessage
}

func (s *stubWebhooksSection) Section() string { return "webhooks" }

func (s *stubWebhooksSection) Values(context.Context) (json.RawMessage, error) {
	if s.valuesErr != nil {
		return nil, s.valuesErr
	}

	return json.Marshal(struct {
		RetryCount int `json:"retry_count"`
	}{RetryCount: s.retryCount})
}

func (s *stubWebhooksSection) Update(_ context.Context, section json.RawMessage) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updateCalls = append(s.updateCalls, section)

	var in struct {
		RetryCount *int `json:"retry_count"`
	}
	if err := json.Unmarshal(section, &in); err != nil {
		return err
	}
	if in.RetryCount != nil {
		s.retryCount = *in.RetryCount
	}

	return nil
}

func (s *stubWebhooksSection) Replace(_ context.Context, section json.RawMessage) error {
	if s.replaceErr != nil {
		return s.replaceErr
	}
	s.replaceCalls = append(s.replaceCalls, section)

	s.retryCount = 0
	if len(section) == 0 {
		return nil
	}

	var in struct {
		RetryCount *int `json:"retry_count"`
	}
	if err := json.Unmarshal(section, &in); err != nil {
		return err
	}
	if in.RetryCount != nil {
		s.retryCount = *in.RetryCount
	}

	return nil
}

// stubSection is an anonymous grouped section with dumb recorders: it
// exercises aggregation rules that neither module shows alone.
type stubSection struct {
	name   string
	values json.RawMessage
	err    error

	updateCalls  []json.RawMessage
	replaceCalls []json.RawMessage
}

func (s *stubSection) Section() string { return s.name }

func (s *stubSection) Values(context.Context) (json.RawMessage, error) {
	if s.err != nil {
		return nil, s.err
	}

	return s.values, nil
}

func (s *stubSection) Update(_ context.Context, section json.RawMessage) error {
	s.updateCalls = append(s.updateCalls, section)

	return nil
}

func (s *stubSection) Replace(_ context.Context, section json.RawMessage) error {
	s.replaceCalls = append(s.replaceCalls, section)

	return nil
}

// newTestService wires the aggregator over fresh stubs and hands them back
// for assertions.
func newTestService() (*settings.Service, *stubPingSection, *stubWebhooksSection) {
	pingSection := &stubPingSection{}
	webhooksSection := &stubWebhooksSection{}

	return settings.NewService([]settings.Section{pingSection, webhooksSection}),
		pingSection, webhooksSection
}

// TestGetSettings_ProjectsSections pins the read path: effective section
// wire JSON lands in the honored wire sections, the signing key and the
// un-honored sections stay absent.
func TestGetSettings_ProjectsSections(t *testing.T) {
	t.Parallel()

	svc, pingSection, webhooksSection := newTestService()
	pingSection.interval = 30
	webhooksSection.retryCount = 7

	got, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}

	if got.Ping == nil || got.Ping.IntervalSeconds == nil || *got.Ping.IntervalSeconds != 30 {
		t.Errorf("ping interval = %v, want 30", got.Ping)
	}
	if got.Webhooks == nil || got.Webhooks.RetryCount == nil || *got.Webhooks.RetryCount != 7 {
		t.Errorf("webhooks retry = %v, want 7", got.Webhooks)
	}
	if got.Webhooks != nil && got.Webhooks.SigningKey != nil {
		t.Errorf("signing key = %q, want nil (write-only)", *got.Webhooks.SigningKey)
	}
	if got.Encryption != nil || got.Messages != nil || got.Logs != nil ||
		got.Gateway != nil || got.Receiver != nil {
		t.Errorf("un-honored sections populated: %+v", got)
	}
}

// TestGetSettings_SectionErrorPropagates pins the failure path: a section
// read error is wrapped and reported instead of a half-filled view.
func TestGetSettings_SectionErrorPropagates(t *testing.T) {
	t.Parallel()

	svc, _, webhooksSection := newTestService()
	webhooksSection.valuesErr = errors.New("boom")

	if _, err := svc.Get(context.Background()); err == nil {
		t.Fatal("GetSettings error = nil, want section error")
	}
}

// TestGetSettings_UnknownSectionDropped pins the JSON decode rule: a
// grouped section the wire DTO does not model falls away at the decode step
// and never reaches the response.
func TestGetSettings_UnknownSectionDropped(t *testing.T) {
	t.Parallel()

	_, pingSection, webhooksSection := newTestService()
	pingSection.interval = 30
	webhooksSection.retryCount = 7
	extra := &stubSection{
		name:   "future_section",
		values: json.RawMessage(`{"anything":"x"}`),
	}

	sections := []settings.Section{pingSection, webhooksSection, extra}
	svc := settings.NewService(sections)

	got, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	if strings.Contains(string(raw), "future_section") {
		t.Errorf("view %s contains future_section, want dropped at decode", raw)
	}
	if *got.Ping.IntervalSeconds != 30 || *got.Webhooks.RetryCount != 7 {
		t.Errorf("known sections lost: ping=%v webhooks=%v", got.Ping, got.Webhooks)
	}
}

// TestGetSettings_NilContributionOmitted pins the empty-contribution rule:
// a section whose Values returns nothing contributes NOTHING - not even an
// empty wire object - so it can never blank a section another provider
// populated under the same name.
func TestGetSettings_NilContributionOmitted(t *testing.T) {
	t.Parallel()

	_, pingSection, webhooksSection := newTestService()
	pingSection.interval = 30
	webhooksSection.retryCount = 7
	empty := &stubSection{name: "ping"}

	sections := []settings.Section{pingSection, webhooksSection, empty}
	svc := settings.NewService(sections)

	got, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.Ping == nil || got.Ping.IntervalSeconds == nil || *got.Ping.IntervalSeconds != 30 {
		t.Errorf("ping section = %v, want 30 (nil contribution must not clobber)", got.Ping)
	}
}

// TestGetSettings_IncompatibleValueErrors pins the loud-failure rule of the
// JSON conversion: section JSON that cannot decode into the wire DTO is
// reported, never silently coerced or dropped.
func TestGetSettings_IncompatibleValueErrors(t *testing.T) {
	t.Parallel()

	_, pingSection, webhooksSection := newTestService()
	broken := &stubSection{
		name:   "ping",
		values: json.RawMessage(`{"interval_seconds":"not-an-int"}`),
	}

	sections := []settings.Section{pingSection, webhooksSection, broken}
	svc := settings.NewService(sections)

	if _, err := svc.Get(context.Background()); err == nil {
		t.Fatal("GetSettings error = nil, want decode error for incompatible value")
	}
}

// TestGetSettings_InvalidSectionJSONErrors pins the section contract: Values
// must emit valid JSON - the merge fails loudly on bytes that are not JSON.
func TestGetSettings_InvalidSectionJSONErrors(t *testing.T) {
	t.Parallel()

	_, pingSection, webhooksSection := newTestService()
	broken := &stubSection{
		name:   "ping",
		values: json.RawMessage(`not-json`),
	}

	sections := []settings.Section{pingSection, webhooksSection, broken}
	svc := settings.NewService(sections)

	if _, err := svc.Get(context.Background()); err == nil {
		t.Fatal("GetSettings error = nil, want marshal error for invalid section JSON")
	}
}

// TestUpdateSettings_SetsOnlyProvidedFields pins PATCH semantics: the
// provided section is delivered byte-exact to its section, absent sections
// are never touched (nil would mean "clear").
func TestUpdateSettings_SetsOnlyProvidedFields(t *testing.T) {
	t.Parallel()

	svc, pingSection, webhooksSection := newTestService()
	webhooksSection.retryCount = 7

	retry := 3
	got, err := svc.Update(context.Background(), smsgateway.DeviceSettings{
		Webhooks: &smsgateway.SettingsWebhooks{
			RetryCount: &retry,
		},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	if len(pingSection.updateCalls) != 0 {
		t.Errorf("ping Update called %d times, want 0", len(pingSection.updateCalls))
	}
	if len(webhooksSection.updateCalls) != 1 {
		t.Fatalf("webhooks Update calls = %d, want 1", len(webhooksSection.updateCalls))
	}
	if got := string(webhooksSection.updateCalls[0]); got != `{"retry_count":3}` {
		t.Errorf("webhooks Update payload = %s, want {\"retry_count\":3} (no signing key)", got)
	}
	if *got.Webhooks.RetryCount != 3 {
		t.Errorf("response retry = %d, want 3", *got.Webhooks.RetryCount)
	}
}

// TestUpdateSettings_UnhonoredSectionNeverReachesSections pins the write
// drop rule: a section the group does not model (messages) rides along in
// the typed input but is never delivered to any section.
func TestUpdateSettings_UnhonoredSectionNeverReachesSections(t *testing.T) {
	t.Parallel()

	svc, pingSection, webhooksSection := newTestService()
	pingSection.interval = 30
	webhooksSection.retryCount = 7

	minInterval := 1
	got, err := svc.Update(context.Background(), smsgateway.DeviceSettings{
		Messages: &smsgateway.SettingsMessages{
			SendIntervalMin: &minInterval,
		},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if len(pingSection.updateCalls) != 0 || len(webhooksSection.updateCalls) != 0 {
		t.Errorf("unhonored input reached sections: ping=%d webhooks=%d",
			len(pingSection.updateCalls), len(webhooksSection.updateCalls))
	}
	if *got.Ping.IntervalSeconds != 30 || *got.Webhooks.RetryCount != 7 {
		t.Errorf("view changed on unhonored input: ping=%d retry=%d, want 30/7",
			*got.Ping.IntervalSeconds, *got.Webhooks.RetryCount)
	}
}

// TestUpdateSettings_EmptyInputIsNoOp pins the no-op boundary: an empty
// PATCH reaches no section and answers the current effective view.
func TestUpdateSettings_EmptyInputIsNoOp(t *testing.T) {
	t.Parallel()

	svc, pingSection, webhooksSection := newTestService()
	pingSection.interval = 60
	webhooksSection.retryCount = 15

	if _, err := svc.Update(context.Background(), smsgateway.DeviceSettings{}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	if len(pingSection.updateCalls) != 0 || len(webhooksSection.updateCalls) != 0 {
		t.Errorf("sections reached on empty input: ping=%d webhooks=%d",
			len(pingSection.updateCalls), len(webhooksSection.updateCalls))
	}
}

// TestUpdateSettings_SectionWriteErrorPropagates pins the failure path: a
// section write error is wrapped and reported.
func TestUpdateSettings_SectionWriteErrorPropagates(t *testing.T) {
	t.Parallel()

	svc, pingSection, _ := newTestService()
	pingSection.updateErr = errors.New("boom")

	if _, err := svc.Update(context.Background(), smsgateway.DeviceSettings{
		Ping: &smsgateway.SettingsPing{},
	}); err == nil {
		t.Fatal("UpdateSettings error = nil, want section write error")
	}
}

// TestReplaceSettings_ClearsUnprovided pins PUT semantics: EVERY section is
// reached - absent ones as nil (clear -> default resumes), provided ones
// byte-exact; the response is the effective view after the writes.
func TestReplaceSettings_ClearsUnprovided(t *testing.T) {
	t.Parallel()

	svc, pingSection, webhooksSection := newTestService()
	webhooksSection.retryCount = 15

	retry := 7
	got, err := svc.Replace(context.Background(), smsgateway.DeviceSettings{
		Webhooks: &smsgateway.SettingsWebhooks{
			RetryCount: &retry,
		},
	})
	if err != nil {
		t.Fatalf("ReplaceSettings: %v", err)
	}

	if len(pingSection.replaceCalls) != 1 || len(pingSection.replaceCalls[0]) != 0 {
		t.Errorf("ping Replace calls = %v, want one nil/empty (clear)", pingSection.replaceCalls)
	}
	if len(webhooksSection.replaceCalls) != 1 {
		t.Fatalf("webhooks Replace calls = %d, want 1", len(webhooksSection.replaceCalls))
	}
	if got := string(webhooksSection.replaceCalls[0]); got != `{"retry_count":7}` {
		t.Errorf("webhooks Replace payload = %s, want {\"retry_count\":7}", got)
	}
	if *got.Webhooks.RetryCount != 7 {
		t.Errorf("response retry = %d, want 7", *got.Webhooks.RetryCount)
	}
}

// TestReplaceSettings_EmptyInputClearsEverything pins the full reset: an
// empty PUT reaches every section as nil so all configured defaults resume.
func TestReplaceSettings_EmptyInputClearsEverything(t *testing.T) {
	t.Parallel()

	svc, pingSection, webhooksSection := newTestService()

	if _, err := svc.Replace(context.Background(), smsgateway.DeviceSettings{}); err != nil {
		t.Fatalf("ReplaceSettings: %v", err)
	}

	if len(pingSection.replaceCalls) != 1 || len(pingSection.replaceCalls[0]) != 0 {
		t.Errorf("ping Replace calls = %v, want one nil/empty", pingSection.replaceCalls)
	}
	if len(webhooksSection.replaceCalls) != 1 || len(webhooksSection.replaceCalls[0]) != 0 {
		t.Errorf("webhooks Replace calls = %v, want one nil/empty", webhooksSection.replaceCalls)
	}
}

// TestReplaceSettings_SectionWriteErrorPropagates pins the failure path: a
// section replace error is wrapped and reported.
func TestReplaceSettings_SectionWriteErrorPropagates(t *testing.T) {
	t.Parallel()

	svc, _, webhooksSection := newTestService()
	webhooksSection.replaceErr = errors.New("boom")

	if _, err := svc.Replace(context.Background(), smsgateway.DeviceSettings{}); err == nil {
		t.Fatal("ReplaceSettings error = nil, want section write error")
	}
}
