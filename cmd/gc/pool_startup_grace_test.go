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

func containsDrainOrphaned(stdout, name string) bool {
	return strings.Contains(stdout, "Draining session '"+name+"': orphaned")
}
