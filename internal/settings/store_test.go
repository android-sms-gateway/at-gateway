package settings_test

import (
	"context"
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/settings"
)

// TestScoped_PrefixesEveryOperation pins the decorator contract: every access
// through the scoped store lands on "<prefix>.<key>" in the underlying table,
// and short keys stay short for the raw store.
func TestScoped_PrefixesEveryOperation(t *testing.T) {
	t.Parallel()

	repo, _, _ := newPersistence(t)
	ctx := context.Background()
	store := settings.Scoped(repo, "ping")

	if err := store.Set(ctx, "interval_seconds", "30"); err != nil {
		t.Fatalf("scoped Set: %v", err)
	}

	raw, ok, err := repo.Get(ctx, "ping.interval_seconds")
	if err != nil {
		t.Fatalf("raw Get: %v", err)
	}
	if !ok || raw != "30" {
		t.Fatalf("raw row = %q (ok=%v), want ping.interval_seconds=30", raw, ok)
	}

	got, ok, err := store.Get(ctx, "interval_seconds")
	if err != nil {
		t.Fatalf("scoped Get: %v", err)
	}
	if !ok || got != "30" {
		t.Fatalf("scoped value = %q (ok=%v), want 30", got, ok)
	}

	if _, ok, err = store.Get(ctx, "absent"); err != nil {
		t.Fatalf("scoped Get absent: %v", err)
	} else if ok {
		t.Error("absent key reported present")
	}

	if err = store.Delete(ctx, "interval_seconds"); err != nil {
		t.Fatalf("scoped Delete: %v", err)
	}
	if _, ok, err = repo.Get(ctx, "ping.interval_seconds"); err != nil {
		t.Fatalf("raw Get after delete: %v", err)
	} else if ok {
		t.Error("prefixed row survived scoped delete")
	}
}

// TestScoped_NamespacesAreIsolated pins the isolation guarantee: two scopes
// using the same short key never see or touch each other's rows.
func TestScoped_NamespacesAreIsolated(t *testing.T) {
	t.Parallel()

	repo, _, _ := newPersistence(t)
	ctx := context.Background()
	ping := settings.Scoped(repo, "ping")
	webhooks := settings.Scoped(repo, "webhooks")

	if err := ping.Set(ctx, "key", "ping-value"); err != nil {
		t.Fatalf("ping Set: %v", err)
	}
	if err := webhooks.Set(ctx, "key", "webhooks-value"); err != nil {
		t.Fatalf("webhooks Set: %v", err)
	}

	if got, ok, err := ping.Get(ctx, "key"); err != nil || !ok || got != "ping-value" {
		t.Fatalf("ping value = %q (ok=%v, err=%v), want ping-value", got, ok, err)
	}
	if got, ok, err := webhooks.Get(ctx, "key"); err != nil || !ok || got != "webhooks-value" {
		t.Fatalf("webhooks value = %q (ok=%v, err=%v), want webhooks-value", got, ok, err)
	}

	if err := ping.Delete(ctx, "key"); err != nil {
		t.Fatalf("ping Delete: %v", err)
	}
	if got, ok, err := webhooks.Get(ctx, "key"); err != nil || !ok || got != "webhooks-value" {
		t.Fatalf("webhooks value after ping delete = %q (ok=%v, err=%v), want untouched", got, ok, err)
	}
}
