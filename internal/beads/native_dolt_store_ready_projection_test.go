package beads

import (
	"context"
	"path/filepath"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreReadyProjectionFillsIsBlocked pins that a cache primed
// over the native store carries bd's stored is_blocked column: an open bead
// with an open blocker reads blocked, an unrelated open bead reads unblocked,
// and neither is left at the nil fallback.
func TestNativeDoltStoreReadyProjectionFillsIsBlocked(t *testing.T) {
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "ready-projection-test", "gc")

	blocker, err := store.Create(Bead{Type: "task", Title: "blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	blocked, err := store.Create(Bead{Type: "task", Title: "blocked"})
	if err != nil {
		t.Fatalf("Create blocked: %v", err)
	}
	if err := store.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	items, err := store.List(ListQuery{Status: "open", TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	enriched, err := store.enrichReadyProjectionForCache(items)
	if err != nil {
		t.Fatalf("enrichReadyProjectionForCache: %v", err)
	}
	got := map[string]*bool{}
	for _, b := range enriched {
		got[b.ID] = b.IsBlocked
	}
	for _, id := range []string{blocker.ID, blocked.ID} {
		if got[id] == nil {
			t.Fatalf("%s: IsBlocked still nil after enrichment (cache would fall back to the weaker predicate)", id)
		}
	}
	if *got[blocker.ID] {
		t.Fatalf("blocker (no deps) enriched as blocked")
	}
	if !*got[blocked.ID] {
		t.Fatalf("blocked (open blocker) enriched as unblocked")
	}
}
