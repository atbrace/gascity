package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// poolSessionWithAssignedWork builds a pool-managed ephemeral session bead and a
// single-store city config, then creates one open work bead assigned to that
// session. When blocked is true the work carries a non-closed "blocks"
// dependency, so it is present in List(open) but excluded from Ready() — i.e.
// open-but-not-actionable, the exact shape that drove the sys-exbu furiosa
// drain<->wake treadmill.
func poolSessionWithAssignedWork(t *testing.T, blocked bool) (string, *config.City, beads.Store, beads.Bead) {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "polecat"}}, // city-scoped: storeRef == "" -> use passed store
	}
	store := beads.NewMemStore()
	// MemStore.Create assigns its own IDs, so capture the returned beads and use
	// their store IDs for assignee and dependency wiring.
	session, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat-gc-vm68",
			"pool_slot":    "1", // isPoolManagedSessionBead == true
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if blocked {
		blocker, err := store.Create(beads.Bead{Type: "task", Status: "open"})
		if err != nil {
			t.Fatalf("create blocker bead: %v", err)
		}
		if err := store.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("DepAdd: %v", err)
		}
	}
	return cityPath, cfg, store, session
}

// TestCloseSessionBeadClosesPoolSessionWithOnlyBlockedWork_sysexbu is the
// regression test for the sys-exbu drain<->wake treadmill. A pool-managed
// session whose only assigned work is open-but-blocked (non-actionable) can
// never be made awake by that work, so the close gate must not let the work
// veto the close. Before the fix the guard used the "open" predicate and
// refused to close (returned false), leaving the session bead alive to be
// re-woken and re-drained forever.
func TestCloseSessionBeadClosesPoolSessionWithOnlyBlockedWork_sysexbu(t *testing.T) {
	cityPath, cfg, store, session := poolSessionWithAssignedWork(t, true /* blocked */)

	closed := closeSessionBeadIfReachableStoreUnassigned(cityPath, cfg, store, nil, session, "drained", time.Now().UTC(), io.Discard)
	if !closed {
		t.Fatal("pool-managed session with only open-but-blocked (non-actionable) assigned work should close; " +
			"got not-closed — the sys-exbu drain<->wake treadmill (guard used the open predicate instead of awake)")
	}

	// Control: with actionable (open + Ready) assigned work, the session must
	// NOT close — proves the fix gates on actionability, not "always close".
	cityPath2, cfg2, store2, session2 := poolSessionWithAssignedWork(t, false /* ready */)
	if closeSessionBeadIfReachableStoreUnassigned(cityPath2, cfg2, store2, nil, session2, "drained", time.Now().UTC(), io.Discard) {
		t.Fatal("pool-managed session with actionable (ready) assigned work must not close")
	}
}
