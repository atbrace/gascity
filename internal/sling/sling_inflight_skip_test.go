package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// inFlightPoolSetup builds a sling against a worker pool for a bead one of that
// pool's own slots has already claimed: status=in_progress, assignee=<slot>,
// and gc.routed_to CLEARED — which is what a claim does, and which is exactly
// why the routed-to idempotency check cannot see the claim. Reproduces
// sys-dpeoz / sys-261l1f.2.
func inFlightPoolSetup(t *testing.T, reassign bool) (SlingOpts, SlingDeps, beads.Bead) {
	t.Helper()
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/myrig", Prefix: "gc"},
		},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(2)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	bead, err := deps.Store.Create(beads.Bead{Title: "already being worked", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inProgress, slot := "in_progress", "myrig/polecat-1"
	if err := deps.Store.Update(bead.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &slot}); err != nil {
		t.Fatalf("Update to claimed state: %v", err)
	}
	opts := SlingOpts{Target: a, BeadOrFormula: bead.ID, NoFormula: true, Reassign: reassign}
	return opts, deps, bead
}

// TestDoSling_SkipsBeadAlreadyInFlight is the regression test for sys-dpeoz.
// A pool slot claims a bead; the claim clears gc.routed_to, so a second
// `gc sling` of the same bead saw no routing state and poured a duplicate
// molecule into another seat. Measured four times in one day on the sysadmin
// rig. A claimed bead must skip instead, and must say who holds it.
func TestDoSling_SkipsBeadAlreadyInFlight(t *testing.T) {
	opts, deps, bead := inFlightPoolSetup(t, false)

	result, err := DoSling(opts, deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if !result.Idempotent {
		t.Fatalf("Idempotent = false, want true: a bead assigned to %q is in flight and must not be re-poured", "myrig/polecat-1")
	}
	if result.InFlightOwner != "myrig/polecat-1" {
		t.Errorf("InFlightOwner = %q, want %q", result.InFlightOwner, "myrig/polecat-1")
	}

	got, err := deps.Store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", bead.ID, err)
	}
	if routed := got.Metadata[beadmeta.RoutedToMetadataKey]; routed != "" {
		t.Errorf("gc.routed_to = %q, want empty: the skip must not route", routed)
	}
	if got.Assignee != "myrig/polecat-1" {
		t.Errorf("Assignee = %q, want it untouched at %q", got.Assignee, "myrig/polecat-1")
	}
}

// TestDoSling_ReassignOverridesInFlightSkip: --reassign is the deliberate
// "take this bead from its current owner" verb, so the in-flight skip must not
// block it. Without this the #3231 order-handoff path would regress.
func TestDoSling_ReassignOverridesInFlightSkip(t *testing.T) {
	opts, deps, bead := inFlightPoolSetup(t, true)

	result, err := DoSling(opts, deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling --reassign: %v", err)
	}
	if result.Idempotent {
		t.Fatalf("Idempotent = true, want false: --reassign must route through the in-flight skip")
	}

	got, err := deps.Store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", bead.ID, err)
	}
	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want cleared by --reassign", got.Assignee)
	}
}

// TestCheckBeadState_InFlightWhenRoutedHereButAssignedElsewhere covers the
// other half of sys-dpeoz: gc.routed_to still names this target, but the bead
// has since been handed to someone else (typically the refinery, holding a
// submitted branch). That is in flight too, and re-slinging it pours a second
// molecule for work already submitted — sys-4zbaym.
func TestCheckBeadState_InFlightWhenRoutedHereButAssignedElsewhere(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs:      []config.Rig{{Name: "myrig", Path: "/myrig", Prefix: "gc"}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(2)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	bead, err := deps.Store.Create(beads.Bead{Title: "submitted work", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	refinery := "myrig/gastown.refinery"
	if err := deps.Store.Update(bead.ID, beads.UpdateOpts{
		Assignee: &refinery,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: a.QualifiedName()},
	}); err != nil {
		t.Fatalf("Update to refinery-held state: %v", err)
	}

	check := CheckBeadStateWithOptions(deps.Store, bead.ID, a, deps, BeadCheckOptions{})
	if !check.Idempotent {
		t.Fatalf("Idempotent = false, want true for a bead held by %q", refinery)
	}
	if check.InFlightOwner != refinery {
		t.Errorf("InFlightOwner = %q, want %q", check.InFlightOwner, refinery)
	}
}
