package beads

import (
	"context"
	"database/sql"
	"fmt"

	beadslib "github.com/steveyegge/beads"
)

// NativeDoltStore hands readiness to the cache — listIncludesCompleteDependencies
// reports true, which latches depsComplete — so it must also supply the column
// that makes the cache's answer equal its own. Without this method every
// Bead.IsBlocked is nil (beadFromNativeIssue cannot set it; beadslib's Issue
// carries no is_blocked field) and cachedBeadReady falls back to the bead's
// OWN direct blocking deps, which misses is_blocked's transitive propagation
// down parent-child edges and any edge onto a row outside this scope. Either
// gap offers the control dispatcher a step whose gate has not opened.
var _ readyProjectionEnrichmentStore = (*NativeDoltStore)(nil)

// enrichReadyProjectionForCache fills bd's denormalized is_blocked column
// onto items, the same readyProjectionSQL scan BdStore spends a `bd sql`
// subprocess on, issued in-process over the storage's raw DB (the
// rawDBGetter hatch repairIDDefault uses) and inside withReadRetry. A
// storage that exposes no raw DB returns an error rather than a silently
// weaker cache: PrimeActive records it as a partial-prime problem, the
// CachingStore then declines every readiness handle and takes the backing's
// own Ready. Slower, and correct.
func (s *NativeDoltStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	if len(items) == 0 {
		return items, nil
	}
	wanted := 0
	for _, item := range items {
		if !skipNativeReadyProjectionEnrichment(item) {
			wanted++
		}
	}
	if wanted == 0 {
		return items, nil
	}

	var projection map[string]bool
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		accessor, ok := storage.(rawDBGetter)
		if !ok || accessor.DB() == nil {
			return fmt.Errorf("native ready projection: storage %T exposes no raw DB", storage)
		}
		rows, err := readyProjectionFromDB(ctx, accessor.DB())
		if err != nil {
			return fmt.Errorf("native ready projection: %w", err)
		}
		projection = rows
		return nil
	})
	if err != nil {
		return items, err
	}

	enriched := make([]Bead, len(items))
	copy(enriched, items)
	for i := range enriched {
		if skipNativeReadyProjectionEnrichment(enriched[i]) {
			continue
		}
		blocked, ok := projection[enriched[i].ID]
		if !ok {
			// A row this store listed and then could not find raced out of
			// the ledger; leave its last value rather than inventing a
			// verdict, matching the bd path exactly.
			continue
		}
		enriched[i].IsBlocked = cloneBoolPtr(&blocked)
	}
	return enriched, nil
}

// skipNativeReadyProjectionEnrichment mirrors the bd path's exclusions:
// closed rows, rows already carrying the column, and message beads, whose
// is_blocked flaps NULL<->false and would make the reconciler re-emit
// bead.updated every cycle.
func skipNativeReadyProjectionEnrichment(item Bead) bool {
	return item.ID == "" || item.Status == "closed" || item.IsBlocked != nil || item.Type == "message"
}

// readyProjectionFromDB scans readyProjectionSQL into id -> is_blocked. A
// NULL is_blocked (bd writes it for some wisps) is absent from the map, the
// same as the bd path's optionalBool leaving the bead's value untouched.
func readyProjectionFromDB(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, readyProjectionSQL())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		var blocked sql.NullBool
		if err := rows.Scan(&id, &blocked); err != nil {
			return nil, err
		}
		if blocked.Valid {
			out[id] = blocked.Bool
		}
	}
	return out, rows.Err()
}
