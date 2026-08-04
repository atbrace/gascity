package beads

import (
	"fmt"
	"strings"
	"testing"
)

// fingerprintRunner answers the fingerprint probe with body and records every
// command it was asked to run, so a test can assert the probe is ONE bd call.
func fingerprintRunner(body string) (CommandRunner, *[]string) {
	var calls []string
	runner := func(_, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		calls = append(calls, joined)
		if len(args) > 0 && args[0] == "sql" {
			return []byte(body), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", joined)
	}
	return runner, &calls
}

func TestActiveFingerprintIsOneCallAndStableForIdenticalState(t *testing.T) {
	runner, calls := fingerprintRunner(`[{"n":157,"mx":"2026-08-04 16:25:07","nb":4}]`)
	s := NewBdStore("/city", runner)

	first, err := s.ActiveFingerprint()
	if err != nil {
		t.Fatalf("ActiveFingerprint: %v", err)
	}
	if first == "" {
		t.Fatalf("ActiveFingerprint returned empty, want a token")
	}
	if len(*calls) != 1 {
		t.Fatalf("bd calls = %d (%v), want exactly 1 -- the probe's whole purpose is replacing a six-call battery", len(*calls), *calls)
	}

	second, err := s.ActiveFingerprint()
	if err != nil {
		t.Fatalf("ActiveFingerprint (second): %v", err)
	}
	if second != first {
		t.Fatalf("fingerprint for unchanged state = %q then %q, want equal", first, second)
	}
}

// TestActiveFingerprintChangesForEachTrackedComponent is the correctness core:
// each component must move the fingerprint on its own, because each catches a
// class of change the others miss. In particular nb (the blocked count) is
// what catches a dependency edit, which changes readiness without necessarily
// touching updated_at.
func TestActiveFingerprintChangesForEachTrackedComponent(t *testing.T) {
	base := `[{"n":157,"mx":"2026-08-04 16:25:07","nb":4}]`
	baseRunner, _ := fingerprintRunner(base)
	baseline, err := NewBdStore("/city", baseRunner).ActiveFingerprint()
	if err != nil {
		t.Fatalf("baseline ActiveFingerprint: %v", err)
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"bead closed or created (count)", `[{"n":156,"mx":"2026-08-04 16:25:07","nb":4}]`},
		{"bead edited in place (max updated_at)", `[{"n":157,"mx":"2026-08-04 16:25:09","nb":4}]`},
		{"dependency edited (blocked count)", `[{"n":157,"mx":"2026-08-04 16:25:07","nb":5}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, _ := fingerprintRunner(tc.body)
			got, err := NewBdStore("/city", runner).ActiveFingerprint()
			if err != nil {
				t.Fatalf("ActiveFingerprint: %v", err)
			}
			if got == baseline {
				t.Fatalf("fingerprint unchanged (%q) after %s; a missed change means a stale ready snapshot gets served", got, tc.name)
			}
		})
	}
}

func TestActiveFingerprintErrorsRatherThanReturningAMatchableToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"no rows", `[]`},
		{"multiple rows", `[{"n":1,"mx":"a","nb":0},{"n":2,"mx":"b","nb":0}]`},
		{"missing column", `[{"n":157,"mx":"2026-08-04 16:25:07"}]`},
		{"not json", `bd: syntax error`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, _ := fingerprintRunner(tc.body)
			got, err := NewBdStore("/city", runner).ActiveFingerprint()
			if err == nil {
				t.Fatalf("ActiveFingerprint returned %q with nil error; a malformed probe must fail loudly so the caller rebuilds", got)
			}
			if got != "" {
				t.Fatalf("ActiveFingerprint returned %q alongside an error, want empty", got)
			}
		})
	}
}

// TestActiveFingerprintToleratesDecimalRenderedAsString pins the reason the
// implementation compares raw JSON: if bd ever renders the DECIMAL sum as a
// string, that must read as "changed" (a wasted rebuild, which is today's
// cost) and never as "unchanged".
func TestActiveFingerprintToleratesDecimalRenderedAsString(t *testing.T) {
	numberRunner, _ := fingerprintRunner(`[{"n":157,"mx":"2026-08-04 16:25:07","nb":4}]`)
	asNumber, err := NewBdStore("/city", numberRunner).ActiveFingerprint()
	if err != nil {
		t.Fatalf("ActiveFingerprint (number): %v", err)
	}

	stringRunner, _ := fingerprintRunner(`[{"n":157,"mx":"2026-08-04 16:25:07","nb":"4"}]`)
	asString, err := NewBdStore("/city", stringRunner).ActiveFingerprint()
	if err != nil {
		t.Fatalf("ActiveFingerprint (string): %v", err)
	}
	if asString == "" {
		t.Fatalf("ActiveFingerprint returned empty for a string-rendered sum, want a usable token")
	}
	if asString == asNumber {
		t.Fatalf("string- and number-rendered sums produced the same token; the comparison must be conservative, not clever")
	}
}
