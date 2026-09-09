package beads

import (
	"context"
	"path/filepath"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreActiveFingerprintTracksActiveSurface pins the contract
// the control-ready cache relies on: the token is stable while nothing
// changes and moves on a create and on a close (the count component), so a
// matching token never hides a readiness change.
func TestNativeDoltStoreActiveFingerprintTracksActiveSurface(t *testing.T) {
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
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "fingerprint-test", "gc")
	var _ ActiveFingerprinter = store

	first, err := store.ActiveFingerprint()
	if err != nil {
		t.Fatalf("ActiveFingerprint (empty): %v", err)
	}
	again, err := store.ActiveFingerprint()
	if err != nil {
		t.Fatalf("ActiveFingerprint (repeat): %v", err)
	}
	if first == "" || first != again {
		t.Fatalf("fingerprint unstable on an unchanged store: %q then %q", first, again)
	}

	bead, err := store.Create(Bead{Title: "fingerprint bead"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	afterCreate, err := store.ActiveFingerprint()
	if err != nil {
		t.Fatalf("ActiveFingerprint (after create): %v", err)
	}
	if afterCreate == first {
		t.Fatalf("fingerprint did not move on create: %q", afterCreate)
	}

	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	afterClose, err := store.ActiveFingerprint()
	if err != nil {
		t.Fatalf("ActiveFingerprint (after close): %v", err)
	}
	if afterClose == afterCreate {
		t.Fatalf("fingerprint did not move on close: %q", afterClose)
	}
}
