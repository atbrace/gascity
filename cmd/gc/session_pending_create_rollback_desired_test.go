package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The pending-create rollback tests in session_lifecycle_chaos_test.go all drive
// setDesired(false), so they only exercise the !desired rollback
// (session_reconciler.go ~1686). The tests below cover the DESIRED branch
// (~2229) — the path that matters for a session that is supposed to be running,
// which is the shape a wedged never-started create would take.
//
// newSessionChaosHarness wires the Manager with the harness's own clock.Fake
// (session_lifecycle_chaos_test.go), so a harness-minted intent's
// pending_create_started_at anchors on the same clock the reconciler reads —
// no manual re-anchoring needed.

// desiredPendingCreateMaxTicks bounds runDesiredPendingCreateTicks: every
// caller wants the same generous one-minute-step ceiling, so it is a
// constant rather than a parameter no caller varies.
const desiredPendingCreateMaxTicks = 30

// runDesiredPendingCreateTicks reconciles up to desiredPendingCreateMaxTicks
// one-minute steps and returns the tick at which the pending-create claim was
// released, or -1.
func runDesiredPendingCreateTicks(t *testing.T, h *sessionChaosHarness) int {
	t.Helper()
	for i := 1; i <= desiredPendingCreateMaxTicks; i++ {
		h.reconcileTick()
		h.env.clk.Advance(time.Minute)
		got, err := h.env.store.Get(h.sessionID)
		if err != nil {
			t.Fatalf("store.Get(%s): %v", h.sessionID, err)
		}
		if got.Status == "closed" || strings.TrimSpace(got.Metadata["pending_create_claim"]) == "" {
			t.Logf("claim released at tick %d (%s): status=%q state=%q",
				i, time.Duration(i)*time.Minute, got.Status,
				strings.TrimSpace(got.Metadata["state"]))
			return i
		}
	}
	return -1
}

// TestDesiredPendingCreateRollsBackWhenStartKeepsFailing pins that a desired
// never-started create whose provider Start never succeeds does not retain its
// pending_create_claim. Without this, the bead holds its alias and a capacity
// slot (BaseStateStartPending counts against cap) with no live runtime. The
// observed mechanism is the failed-create rollback at the first tick
// (status=closed, state=failed-create), not the never-started lease — this test
// pins that the claim does not persist on the desired branch, not lease-floor
// timing.
func TestDesiredPendingCreateRollsBackWhenStartKeepsFailing(t *testing.T) {
	h := newSessionChaosHarness(t, 20260729)
	h.createSessionIntent()
	h.assertCreatingIntent()

	h.env.sp.StartErrors[h.sessionName] = errors.New("provider start failure")

	if at := runDesiredPendingCreateTicks(t, h); at < 0 {
		got, _ := h.env.store.Get(h.sessionID)
		t.Fatalf("desired pending-create still claimed after 30m: status=%q state=%q claim=%q running=%v",
			got.Status,
			strings.TrimSpace(got.Metadata["state"]),
			strings.TrimSpace(got.Metadata["pending_create_claim"]),
			h.env.sp.IsRunning(h.sessionName))
	}
}

// TestDesiredQuarantinedPendingCreateRollsBackAfterLeaseExpiry (upstream #4822)
// is not carried: this lineage has no quarantined-pending-create lease-expiry
// rollback, and the test encodes that behaviour, not #6016 (sys-by2243.12).
func TestDesiredCreatingPendingCreateReleasesClaim(t *testing.T) {
	h := newSessionChaosHarness(t, 20260730)
	h.createSessionIntent()

	if err := h.env.store.SetMetadataBatch(h.sessionID, map[string]string{
		"state":                string(sessionpkg.StateCreating),
		"pending_create_claim": "true",
		"last_woke_at":         "",
	}); err != nil {
		t.Fatalf("seed creating shape: %v", err)
	}
	h.env.sp.StartErrors[h.sessionName] = errors.New("provider start failure")

	if at := runDesiredPendingCreateTicks(t, h); at < 0 {
		got, _ := h.env.store.Get(h.sessionID)
		t.Fatalf("creating+claim+never-started survived 30m: status=%q state=%q claim=%q",
			got.Status,
			strings.TrimSpace(got.Metadata["state"]),
			strings.TrimSpace(got.Metadata["pending_create_claim"]))
	}
}
