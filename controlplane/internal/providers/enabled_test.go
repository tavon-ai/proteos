package providers_test

import (
	"context"
	"testing"

	"github.com/tavon/proteos/controlplane/internal/providers"
	"github.com/tavon/proteos/controlplane/internal/testutil"
)

// TestSetEnabledReconciles proves SetEnabled enables exactly the listed providers
// and disables the rest, and reports unknown keys. It restores the seeded state
// on cleanup (testutil does not truncate the shared providers table).
func TestSetEnabledReconciles(t *testing.T) {
	ctx := context.Background()
	_, q := testutil.Postgres(t)
	reg := providers.NewRegistry(q)

	// Restore all four seeds to enabled on cleanup so other tests on the shared
	// CI database see the pristine registry.
	t.Cleanup(func() {
		_, _ = reg.SetEnabled(context.Background(), []string{"claude", "gemini", "openai", "pi"})
	})

	unknown, err := reg.SetEnabled(ctx, []string{"claude", "pi", "ghost"})
	if err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if len(unknown) != 1 || unknown[0] != "ghost" {
		t.Fatalf("unknown keys = %v, want [ghost]", unknown)
	}

	got := map[string]bool{}
	list, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		got[p.Key] = p.Enabled
	}
	for _, k := range []string{"claude", "pi"} {
		if !got[k] {
			t.Fatalf("%s should be enabled after reconcile: %v", k, got)
		}
	}
	for _, k := range []string{"gemini", "openai"} {
		if got[k] {
			t.Fatalf("%s should be disabled after reconcile: %v", k, got)
		}
	}
}
