package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// reconcileWithScaleCheck runs the reconciler passing scaleCheckCounts — the demand
// signal the sys-exbu v6 pool startup grace gates on (result.ScaleCheckCounts, the
// only demand reliably present at the tick a fresh pool session is orphan-evaluated;
// poolDesired/desiredState were both observed empty at drain-time live). The shallow
// reconcileWithPoolDesired helper routes through reconcileSessionBeads, which passes
// scaleCheckCounts=nil, so grace tests MUST use this helper to exercise the guard.
func (e *reconcilerTestEnv) reconcileWithScaleCheck(sessions []beads.Bead, scaleCheckCounts map[string]int) int {
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	return reconcileSessionBeadsAtPathWithNamedDemand(
		context.Background(), "", sessions, e.desiredState, cfgNames, e.cfg, e.sp,
		e.store, nil, nil, nil, nil, e.dt, nil, nil, nil, scaleCheckCounts, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr,
	)
}

// makeManagedSlotZeroPoolSession builds the TRUE production shape that disproved
// v1 AND v2 (sys-exbu): a canonical-singleton (max_active_sessions=1) pool session
// runs at canonical SLOT 0, so session_beads.go stamps pool_managed=true but NO
// pool_slot (that metadata is written only when poolSlot>0). Both prior grace
// fixes gated on pool_slot and were therefore structurally dead for this shape.
// Freshness is expressed via pending_create_started_at (the reliable spawn stamp)
// rather than overriding CreatedAt — proving the grace no longer depends on the
// stale bead-row CreatedAt.
func makeManagedSlotZeroPoolSession(t *testing.T, env *reconcilerTestEnv, name, template string, ageFromNow time.Duration) beads.Bead {
	t.Helper()
	session := env.createSessionBead(name, template)
	env.setSessionMetadata(&session, map[string]string{
		"pool_managed":              "true",
		"session_origin":            "ephemeral",
		"pending_create_started_at": env.clk.Now().UTC().Add(ageFromNow).Format(time.RFC3339),
	})
	env.markSessionActive(&session)
	if err := env.sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(%s): %v", name, err)
	}
	if err := env.sp.SetMeta(name, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	return session
}

// TestReconcileSessionBeads_FreshSlotZeroPoolSessionSurvives is the sys-exbu v6
// regression, modeling the exact production shape prior fixes missed: a fresh
// canonical-singleton pool session with pool_managed=true but pool_slot ABSENT,
// whose demand is present ONLY as a live scale-check count (poolDesired/desiredState
// empty at drain-time — the v3/v4 failure). v6 identifies pool membership by
// pool_managed and sources demand from scaleCheckCounts, so the fresh session is
// protected through its claim window.
func TestReconcileSessionBeads_FreshSlotZeroPoolSessionSurvives(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "muthur"}}}

	// The concrete freshly-spawned slot-0 session: pool_managed, NO pool_slot,
	// NOT keyed in desiredState, fresh (10s < 60s grace).
	session := makeManagedSlotZeroPoolSession(t, env, "muthur-gc-abcd", "muthur", -10*time.Second)

	// Demand lives ONLY in scaleCheckCounts (production reality at drain-time).
	woken := env.reconcileWithScaleCheck([]beads.Bead{session}, map[string]int{"muthur": 1})
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	if got := env.stdout.String(); containsDrainOrphaned(got, "muthur-gc-abcd") {
		t.Fatalf("fresh slot-0 pool session (no pool_slot) was orphan-drained — the disproof shape:\nstdout=%s", got)
	}
	if !env.sp.IsRunning("muthur-gc-abcd") {
		t.Fatalf("fresh slot-0 pool session runtime was stopped; expected it kept alive during startup grace")
	}
}

// TestReconcileSessionBeads_StaleSlotZeroPoolSessionDrains proves the v6 grace is
// still bounded for the slot-0 shape: past the startup window (freshness measured
// from pending_create_started_at) an unclaimed pool session IS drained even while
// scale-check demand persists.
func TestReconcileSessionBeads_StaleSlotZeroPoolSessionDrains(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "muthur"}}}

	// Stale: spawned 90s ago, past the 60s default startup grace.
	session := makeManagedSlotZeroPoolSession(t, env, "muthur-gc-stale0", "muthur", -90*time.Second)

	_ = env.reconcileWithScaleCheck([]beads.Bead{session}, map[string]int{"muthur": 1})

	if got := env.stdout.String(); !containsDrainOrphaned(got, "muthur-gc-stale0") {
		t.Fatalf("stale slot-0 pool session past startup grace was NOT drained:\nstdout=%s", got)
	}
}

// TestReconcileSessionBeads_FreshSlotZeroPoolSessionWithoutDemandDrains is the v6
// CRITICAL invariant: a fresh slot-0 pool session whose template's scale-check
// returns 0 (a confirmed-unwanted pool) is a genuine orphan and IS drained. This
// is what killed v5 (freshness-only grace wrongly protected the fresh-but-unwanted
// helper) — mirrors TestCityRuntimeBeadReconcileTick_ScaleCheckPartialKeepsOnlyAffectedPoolSession.
func TestReconcileSessionBeads_FreshSlotZeroPoolSessionWithoutDemandDrains(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "muthur"}}}

	session := makeManagedSlotZeroPoolSession(t, env, "muthur-gc-nodemand0", "muthur", -10*time.Second)

	// scale-check returns 0 -> confirmed unwanted -> no grace -> drains.
	_ = env.reconcileWithScaleCheck([]beads.Bead{session}, map[string]int{"muthur": 0})

	if got := env.stdout.String(); !containsDrainOrphaned(got, "muthur-gc-nodemand0") {
		t.Fatalf("fresh slot-0 pool session with scale_check=0 was NOT drained:\nstdout=%s", got)
	}
}

// TestPoolSessionScaleCheckDemand_MatchesQualifiedTemplateKey models the exbudiag3
// live shape (sys-exbu): the running muthur-gc-XXXX bead carried its template as the
// rig-qualified string "sysadmin/muthur" and result.ScaleCheckCounts was keyed the
// same way, so the demand probe matches by raw key. The canonical-resolution branch
// (canonicalTemplateKeyForName) additionally covers the case where a bindingless
// bare name resolves through findAgentByTemplate to the agent's QualifiedName.
func TestPoolSessionScaleCheckDemand_MatchesQualifiedTemplateKey(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "muthur", Dir: "sysadmin"}}}
	// Session carries the rig-qualified template exactly as the live bead did
	// (DIAG: tmpl="sysadmin/muthur"); demand map keyed the same way.
	session := beads.Bead{Metadata: map[string]string{
		"session_name": "muthur-gc-abcd",
		"template":     "sysadmin/muthur",
		"pool_managed": "true",
	}}
	if got := poolSessionScaleCheckDemand(session, cfg, map[string]int{"sysadmin/muthur": 3}); got != 3 {
		t.Fatalf("poolSessionScaleCheckDemand = %d, want 3 (qualified-key match)", got)
	}
	// A confirmed-zero-demand pool returns 0 (drains).
	if got := poolSessionScaleCheckDemand(session, cfg, map[string]int{"sysadmin/muthur": 0}); got != 0 {
		t.Fatalf("poolSessionScaleCheckDemand = %d, want 0 for scale_check=0", got)
	}
	// No demand map at all -> 0.
	if got := poolSessionScaleCheckDemand(session, cfg, nil); got != 0 {
		t.Fatalf("poolSessionScaleCheckDemand = %d, want 0 for nil counts", got)
	}
	// Canonical-resolution branch: a bindingless bare name resolves to the agent's
	// QualifiedName, so a bare-keyed demand map still matches an unqualified agent.
	bareCfg := &config.City{Agents: []config.Agent{{Name: "muthur"}}}
	bareSession := beads.Bead{Metadata: map[string]string{
		"session_name": "muthur-gc-abcd", "template": "muthur", "pool_managed": "true",
	}}
	if got := poolSessionScaleCheckDemand(bareSession, bareCfg, map[string]int{"muthur": 2}); got != 2 {
		t.Fatalf("poolSessionScaleCheckDemand = %d, want 2 (bare-key match)", got)
	}
}

func containsDrainOrphaned(stdout, name string) bool {
	return strings.Contains(stdout, "Draining session '"+name+"': orphaned")
}
