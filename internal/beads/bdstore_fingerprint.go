package beads

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ActiveFingerprinter is implemented by stores that can cheaply summarize their
// active (non-closed) surface, so a caller already holding a primed snapshot
// can detect "nothing changed here" without re-running a full prime battery.
//
// It is deliberately an optional capability rather than part of Store: a store
// that cannot answer it is not broken, it just does not get the cheap path.
// Callers must treat a missing implementation, an error, or an empty result as
// "assume changed" -- never as "unchanged".
type ActiveFingerprinter interface {
	ActiveFingerprint() (string, error)
}

// activeFingerprintSQL summarizes the same active issue+wisp surface
// readyProjectionSQL scans, but aggregates server-side so the answer is three
// scalars instead of one row per bead.
//
// Each component catches a distinct class of change that can move a bead into
// or out of the ready set:
//
//	count(*)        creates, deletes, and closes (a close leaves the set)
//	max(updated_at) in-place edits: status open->in_progress, assignee,
//	                routing metadata, deferral
//	blocked count   dependency edits, which land on the bead as a recomputed
//	                is_blocked and need not touch updated_at
//
// The blocked count is what makes this safe to gate a readiness cache on: a
// dependency added or closed changes readiness without necessarily moving
// either of the other two components.
func activeFingerprintSQL() string {
	return "select count(*) as n, coalesce(max(updated_at),'') as mx, coalesce(sum(case when is_blocked then 1 else 0 end),0) as nb from (select updated_at, is_blocked from issues where status <> 'closed' union all select updated_at, is_blocked from wisps where status <> 'closed') t"
}

// activeFingerprintColumns are compared in a fixed order so the resulting
// fingerprint is stable across bd's JSON object key ordering.
var activeFingerprintColumns = []string{"n", "mx", "nb"}

// ActiveFingerprint returns an opaque token that changes whenever the active
// bead surface changes. It is only ever compared for equality; it carries no
// meaning beyond "same as last time" and must not be parsed by callers.
func (s *BdStore) ActiveFingerprint() (string, error) {
	out, err := s.runner(s.dir, "bd", "sql", activeFingerprintSQL(), "--json")
	if err != nil {
		return "", fmt.Errorf("bd sql active fingerprint: %w", err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(extractJSON(out), &rows); err != nil {
		return "", fmt.Errorf("bd sql active fingerprint: parsing JSON: %w", err)
	}
	if len(rows) != 1 {
		return "", fmt.Errorf("bd sql active fingerprint: got %d rows, want exactly 1", len(rows))
	}

	// Compare the raw JSON of each aggregate rather than parsed numerics. The
	// values only ever need to be equal-or-not, and raw bytes cannot produce a
	// false "unchanged" -- the one unsafe direction -- if bd's rendering of a
	// DECIMAL sum ever shifts between number and string. A rendering change
	// would instead force a rebuild, which is exactly the pre-existing cost.
	var b strings.Builder
	for _, column := range activeFingerprintColumns {
		raw, ok := rows[0][column]
		if !ok {
			return "", fmt.Errorf("bd sql active fingerprint: result missing column %q", column)
		}
		b.Write(raw)
		b.WriteByte('|')
	}
	return b.String(), nil
}
