package settings

import (
	"context"
	"fmt"
)

// Store is a semantics-free string key-value store: keys are opaque to the
// store, values are stored and returned verbatim, and an unset key reports
// ok=false rather than an empty value. What a key means belongs to the
// consuming module, never to the store.
type Store interface {
	// Get returns the stored value for key; ok is false when the key is unset.
	Get(ctx context.Context, key string) (string, bool, error)

	// Set stores value under key, replacing any previous value.
	Set(ctx context.Context, key string, value string) error

	// Delete removes key; deleting an unset key is a no-op.
	Delete(ctx context.Context, key string) error
}

// scoped is a Store restricted to a single key prefix.
type scoped struct {
	inner  Store
	prefix string
}

// Scoped returns inner restricted to prefix: every access of key maps to
// "<prefix>.<key>", so a consumer only ever addresses its own namespace.
func Scoped(inner Store, prefix string) Store {
	return &scoped{inner: inner, prefix: prefix}
}

func (s *scoped) Get(ctx context.Context, key string) (string, bool, error) {
	value, ok, err := s.inner.Get(ctx, s.prefix+"."+key)
	if err != nil {
		return "", false, fmt.Errorf("get: %w", err)
	}

	return value, ok, nil
}

func (s *scoped) Set(ctx context.Context, key string, value string) error {
	if err := s.inner.Set(ctx, s.prefix+"."+key, value); err != nil {
		return fmt.Errorf("set: %w", err)
	}

	return nil
}

func (s *scoped) Delete(ctx context.Context, key string) error {
	if err := s.inner.Delete(ctx, s.prefix+"."+key); err != nil {
		return fmt.Errorf("delete: %w", err)
	}

	return nil
}
