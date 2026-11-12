package settings

import (
	"context"
	"encoding/json"
)

// Section is a module's contribution behind the settings API. Both
// directions travel as RAW wire JSON keyed by the section name: Values
// emits the module's own struct, Update and Replace feed it back in. The
// aggregator never sees a field name or a typed value - parsing, defaults,
// validation and unset semantics stay inside the owning module.
type Section interface {
	// Section returns the wire section name ("ping", "webhooks").
	Section() string

	// Values returns the effective section view as wire JSON (nil when the
	// section has nothing to contribute; the map merge skips it).
	Values(ctx context.Context) (json.RawMessage, error)

	// Update applies a PRESENT section (PATCH semantics): fields the JSON
	// omits leave their stored values untouched.
	Update(ctx context.Context, section json.RawMessage) error

	// Replace overwrites the section (PUT semantics): a nil or empty input
	// clears every field so the configured defaults resume.
	Replace(ctx context.Context, section json.RawMessage) error
}
