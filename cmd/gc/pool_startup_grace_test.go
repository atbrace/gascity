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

// makeRunningOrphanPoolSession builds a live pool session bead that is NOT in
// desiredState and NOT a configured/named session — the exact shape a freshly
// poured pool worker takes during its boot window: the demand tier desires one
// (poolDesired[template] > 0), but the concrete session has not yet run
// `gc hook --claim`, so it holds no concrete-assigned work.
func makeRunningOrphanPoolSession(t *testing.T, env *reconcilerTestEnv, name, template string, ageFromNow time.Duration) beads.Bead {
	t.Helper()
	session := env.createSessionBead(name, template)
	env.setSessionMetadata(&session, map[string]string{
		"pool_slot":     "1",
		"pool_template": template,
	})
	env.markSessionActive(&session)
	// Age the session relative to the fake clock (MemStore stamps real wall
	// time on Create; the reconciler reads CreatedAt off the passed bead).
	session.CreatedAt = env.clk.Now().UTC().Add(ageFromNow)
	// Make providerAlive true: the runtime is running and its GC_SESSION_ID
	// maps back to this session bead.
	if err := env.sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(%s): %v", name, err)
	}
	if err := env.sp.SetMeta(name, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	return session
}

// TestReconcileSessionBeads_FreshPoolSessionSurvivesStartupGrace is the sys-exbu
// regression: a freshly-spawned pool session with outstanding template demand
// must NOT be orphan-drained during its startup/claim window. Before the fix the
// orphan gate drained it every tick (it holds no concrete-assigned work until it
// boots and claims), while the demand tier kept re-desiring one — the respawn
// treadmill.
func TestReconcileSessionBeads_FreshPoolSessionSurvivesStartupGrace(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "muthur"}}}

	// Fresh: created 10s ago, well within the 60s default startup grace.
	session := makeRunningOrphanPoolSession(t, env, "muthur-gc-abcd", "muthur", -10*time.Second)

	woken := env.reconcileWithPoolDesired([]beads.Bead{session}, map[string]int{"muthur": 1})
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	if got := env.stdout.String(); containsDrainOrphaned(got, "muthur-gc-abcd") {
		t.Fatalf("fresh pool session was orphan-drained during startup grace:\nstdout=%s", got)
	}
	if !env.sp.IsRunning("muthur-gc-abcd") {
		t.Fatalf("fresh pool session runtime was stopped; expected it kept alive during startup grace")
	}
}

// TestReconcileSessionBeads_StalePoolSessionStillDrains proves the grace is
// bounded: once a pool session ages past the startup window without claiming, it
// IS orphan-drained (otherwise the guard would be a permanent leak).
func TestReconcileSessionBeads_StalePoolSessionStillDrains(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "muthur"}}}

	// Stale: created 90s ago, past the 60s default startup grace.
	session := makeRunningOrphanPoolSession(t, env, "muthur-gc-stale", "muthur", -90*time.Second)

	_ = env.reconcileWithPoolDesired([]beads.Bead{session}, map[string]int{"muthur": 1})

	if got := env.stdout.String(); !containsDrainOrphaned(got, "muthur-gc-stale") {
		t.Fatalf("stale pool session past startup grace was NOT drained:\nstdout=%s", got)
	}
}

// TestReconcileSessionBeads_FreshPoolSessionWithoutDemandDrains proves the guard
// only protects sessions the demand tier still wants: a fresh pool session whose
// template has zero desired count is a genuine orphan and IS drained.
func TestReconcileSessionBeads_FreshPoolSessionWithoutDemandDrains(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "muthur"}}}

	session := makeRunningOrphanPoolSession(t, env, "muthur-gc-nodemand", "muthur", -10*time.Second)

	// No demand for the template.
	_ = env.reconcileWithPoolDesired([]beads.Bead{session}, map[string]int{})

	if got := env.stdout.String(); !containsDrainOrphaned(got, "muthur-gc-nodemand") {
		t.Fatalf("fresh pool session with no template demand was NOT drained:\nstdout=%s", got)
	}
}

// TestReconcileSessionBeads_FreshPoolSessionSurvivesWithCanonicalSlotInDesiredState
// models the PRODUCTION shape that disproved the v1 fix (sys-exbu): the demand
// tier's desired pool slot is present in desiredState keyed under a canonical /
// pending name ("muthur-pending-xyz"), distinct from the concrete spawned session
// ("muthur-gc-abcd") that is actually alive. v1 counted desired-state entries by
// template (poolTemplateBoundCount): the pending slot made bound==desired, so
// "unmet demand" was always false and the grace never fired — the concrete
// session drained every tick. The live-count discriminator counts ACTUAL live
// pool sessions (1) against poolDesired (1), so the fresh session is protected.
func TestReconcileSessionBeads_FreshPoolSessionSurvivesWithCanonicalSlotInDesiredState(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "muthur"}}}

	// The demand tier's desired slot: present in desiredState under a canonical /
	// pending name, NOT the concrete session's name. running=false so no runtime
	// is registered under the pending name (only the concrete session is alive).
	env.addDesired("muthur-pending-xyz", "muthur", false)

	// The concrete freshly-spawned session, NOT keyed in desiredState.
	session := makeRunningOrphanPoolSession(t, env, "muthur-gc-abcd", "muthur", -10*time.Second)

	woken := env.reconcileWithPoolDesired([]beads.Bead{session}, map[string]int{"muthur": 1})
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	if got := env.stdout.String(); containsDrainOrphaned(got, "muthur-gc-abcd") {
		t.Fatalf("fresh pool session was orphan-drained despite a canonical desired slot (v1 disproof shape):\nstdout=%s", got)
	}
	if !env.sp.IsRunning("muthur-gc-abcd") {
		t.Fatalf("fresh pool session runtime was stopped; expected it kept alive during startup grace")
	}
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

// TestReconcileSessionBeads_FreshSlotZeroPoolSessionSurvives is the sys-exbu v3
// regression, modeling the exact production shape v1+v2 missed: a fresh canonical-
// singleton pool session with pool_managed=true but pool_slot ABSENT, whose only
// demand is a canonical desiredState slot (NOT in the numeric poolDesired map, so
// poolDesired is passed empty). The pool_slot-gated v1/v2 grace never fired for
// this shape; v3 identifies pool membership by pool_managed and sources demand
// from desiredState, so the fresh session is protected through its claim window.
func TestReconcileSessionBeads_FreshSlotZeroPoolSessionSurvives(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "muthur"}}}

	// The demand tier's single desired slot: a desiredState entry keyed under a
	// canonical/pending name, running=false, distinct from the concrete session.
	env.addDesired("muthur-canonical-slot", "muthur", false)

	// The concrete freshly-spawned slot-0 session: pool_managed, NO pool_slot,
	// NOT keyed in desiredState, fresh (10s < 60s grace).
	session := makeManagedSlotZeroPoolSession(t, env, "muthur-gc-abcd", "muthur", -10*time.Second)

	// poolDesired is EMPTY — the production reality for a canonical-singleton pool
	// (its demand lives only in desiredState).
	woken := env.reconcileWithPoolDesired([]beads.Bead{session}, map[string]int{})
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	if got := env.stdout.String(); containsDrainOrphaned(got, "muthur-gc-abcd") {
		t.Fatalf("fresh slot-0 pool session (no pool_slot) was orphan-drained — the v1/v2 disproof shape:\nstdout=%s", got)
	}
	if !env.sp.IsRunning("muthur-gc-abcd") {
		t.Fatalf("fresh slot-0 pool session runtime was stopped; expected it kept alive during startup grace")
	}
}

// TestReconcileSessionBeads_StaleSlotZeroPoolSessionDrains proves the v3 grace is
// still bounded for the slot-0 shape: past the startup window (freshness measured
// from pending_create_started_at) an unclaimed pool session IS drained.
func TestReconcileSessionBeads_StaleSlotZeroPoolSessionDrains(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "muthur"}}}

	env.addDesired("muthur-canonical-slot", "muthur", false)
	// Stale: spawned 90s ago, past the 60s default startup grace.
	session := makeManagedSlotZeroPoolSession(t, env, "muthur-gc-stale0", "muthur", -90*time.Second)

	_ = env.reconcileWithPoolDesired([]beads.Bead{session}, map[string]int{})

	if got := env.stdout.String(); !containsDrainOrphaned(got, "muthur-gc-stale0") {
		t.Fatalf("stale slot-0 pool session past startup grace was NOT drained:\nstdout=%s", got)
	}
}

// TestReconcileSessionBeads_FreshSlotZeroPoolSessionWithoutDemandDrains proves the
// v3 grace still only protects sessions the demand tier wants: a fresh slot-0 pool
// session whose template has NO desiredState slot and empty poolDesired is a
// genuine orphan and IS drained.
func TestReconcileSessionBeads_FreshSlotZeroPoolSessionWithoutDemandDrains(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "muthur"}}}

	// No desiredState slot for muthur, and empty poolDesired -> zero demand.
	session := makeManagedSlotZeroPoolSession(t, env, "muthur-gc-nodemand0", "muthur", -10*time.Second)

	_ = env.reconcileWithPoolDesired([]beads.Bead{session}, map[string]int{})

	if got := env.stdout.String(); !containsDrainOrphaned(got, "muthur-gc-nodemand0") {
		t.Fatalf("fresh slot-0 pool session with no template demand was NOT drained:\nstdout=%s", got)
	}
}

// TestPoolSessionTemplateDemand_ProbesPoolDesiredByRawTemplateKey is the sys-exbu
// v4 regression from the exbudiag3 live capture: v3 built the demand ceiling by
// re-resolving every poolDesired key through findAgentByTemplate, which is
// asymmetric (matches a bare local name but not the rig-qualified form). For a
// rig-qualified pool template the numeric demand key ("sysadmin/muthur") was
// dropped -> desired=0 -> no grace, even though poolDesired carried it =1. v4
// probes poolDesired DIRECTLY with the session's own template-key spellings, so
// the qualified key matches by string and the demand is found.
func TestPoolSessionTemplateDemand_ProbesPoolDesiredByRawTemplateKey(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "muthur", Dir: "sysadmin"}}}
	// Session carries its template as the rig-qualified string, exactly as the
	// live muthur-gc-XXXX bead did (DIAG: tmpl="sysadmin/muthur").
	session := beads.Bead{Metadata: map[string]string{
		"session_name": "muthur-gc-abcd",
		"template":     "sysadmin/muthur",
		"pool_managed": "true",
	}}
	// poolDesired keyed by the rig-qualified name (the shape that yielded desired=0
	// under v3). desiredState empty — demand lives only in poolDesired here.
	poolDesired := map[string]int{"sysadmin/muthur": 1}
	if got := poolSessionTemplateDemand(session, cfg, map[string]TemplateParams{}, poolDesired); got != 1 {
		t.Fatalf("poolSessionTemplateDemand = %d, want 1 (v3 dropped the qualified key to 0)", got)
	}
	// And the capacity gate is satisfied with a single live session.
	if !poolSessionWithinDesiredCapacity(session, cfg, []beads.Bead{session}, map[string]TemplateParams{}, poolDesired) {
		t.Fatal("poolSessionWithinDesiredCapacity = false, want true (1 live <= 1 desired)")
	}
}

func containsDrainOrphaned(stdout, name string) bool {
	return strings.Contains(stdout, "Draining session '"+name+"': orphaned")
}
