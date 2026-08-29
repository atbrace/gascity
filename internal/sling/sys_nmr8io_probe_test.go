package sling

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Probe for sys-nmr8io: does a SECOND real AttachFormula call (no manually
// stamped metadata, no --force) against a bead that already has a live
// graph.v2 workflow attached succeed and create a second live root?
func TestProbeSecondRealAttachOnSameBead(t *testing.T) {
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
		Agents: []config.Agent{slingControlDispatcherAgent()},
	}
	formulatest.EnableV2ForTest(t)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{ID: "BL-42", Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	first, err := s.AttachFormula(context.Background(), "graph-work", source.ID, a, FormulaOpts{})
	if err != nil {
		t.Fatalf("first AttachFormula: %v", err)
	}
	t.Logf("first attach: WorkflowID=%s WispRootID=%s", first.WorkflowID, first.WispRootID)

	second, err := s.AttachFormula(context.Background(), "graph-work", source.ID, a, FormulaOpts{})
	if err != nil {
		t.Logf("second AttachFormula correctly rejected: %v", err)
		return
	}
	t.Logf("second attach: WorkflowID=%s WispRootID=%s", second.WorkflowID, second.WispRootID)

	live := liveGraphV2Roots(t, deps.Store)
	t.Logf("live graph.v2 roots after two attaches: %d", len(live))
	for _, r := range live {
		t.Logf("  root=%s metadata=%v", r.ID, r.Metadata)
	}
	if len(live) > 1 {
		t.Fatalf("BUG CONFIRMED: %d live workflow roots exist for the same source bead %s after two unforced attaches (want 1, or the second attach should have been rejected)", len(live), source.ID)
	}
}
