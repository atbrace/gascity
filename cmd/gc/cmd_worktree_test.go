package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSetupScript writes an executable fake worktree-setup.sh at
// cityPath/packs/gastown/scripts/worktree-setup.sh that records every
// argument it receives (one per line, in order) to recordFile, then exits
// with exitCode. Tests exercise doWorktreeHQ's contract with the script
// (argv shape, exit-code propagation) without invoking the real script.
func fakeSetupScript(t *testing.T, cityPath, recordFile string, exitCode int) {
	t.Helper()
	scriptDir := filepath.Join(cityPath, "packs", "gastown", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\n' \"$a\" >> '%s'; done\nexit %d\n", recordFile, exitCode)
	scriptPath := filepath.Join(scriptDir, "worktree-setup.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readRecordedArgs(t *testing.T, recordFile string) []string {
	t.Helper()
	data, err := os.ReadFile(recordFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	trimmed := strings.TrimSuffix(string(data), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func TestDoWorktreeHQInvokesSetupScriptWithFreshenCommit(t *testing.T) {
	cityDir := t.TempDir()
	recordFile := filepath.Join(t.TempDir(), "argv.txt")
	fakeSetupScript(t, cityDir, recordFile, 0)
	t.Setenv("GC_TEMPLATE", "gascity/builder")
	t.Setenv("GC_AGENT", "")

	var stdout, stderr bytes.Buffer
	path, err := doWorktreeHQ(cityDir, "ga-34q3ss", &stdout, &stderr)
	if err != nil {
		t.Fatalf("doWorktreeHQ() error = %v, stderr = %s", err, stderr.String())
	}

	wantPath := filepath.Join(cityDir, ".gc", "worktrees", "_hq", "builder-ga-34q3ss")
	if path != wantPath {
		t.Errorf("doWorktreeHQ() path = %q, want %q", path, wantPath)
	}

	got := readRecordedArgs(t, recordFile)
	want := []string{cityDir, wantPath, "builder", "--freshen-commit"}
	if len(got) != len(want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q (full argv %q)", i, got[i], want[i], got)
		}
	}
	for _, forbidden := range got {
		if forbidden == "--reset-main" {
			t.Fatalf("argv %q must never contain --reset-main", got)
		}
	}
}

func TestDoWorktreeHQWritesBeadsRedirect(t *testing.T) {
	cityDir := t.TempDir()
	recordFile := filepath.Join(t.TempDir(), "argv.txt")
	fakeSetupScript(t, cityDir, recordFile, 0)
	t.Setenv("GC_TEMPLATE", "gascity/builder")

	var stdout, stderr bytes.Buffer
	path, err := doWorktreeHQ(cityDir, "ga-34q3ss", &stdout, &stderr)
	if err != nil {
		t.Fatalf("doWorktreeHQ() error = %v, stderr = %s", err, stderr.String())
	}

	redirectPath := filepath.Join(path, ".beads", "redirect")
	data, err := os.ReadFile(redirectPath)
	if err != nil {
		t.Fatalf("reading %s: %v", redirectPath, err)
	}
	got := strings.TrimSpace(string(data))
	want := filepath.Join(cityDir, ".beads")
	if got != want {
		t.Errorf(".beads/redirect content = %q, want %q", got, want)
	}
}

func TestDoWorktreeHQIsIdempotent(t *testing.T) {
	cityDir := t.TempDir()
	recordFile := filepath.Join(t.TempDir(), "argv.txt")
	fakeSetupScript(t, cityDir, recordFile, 0)
	t.Setenv("GC_TEMPLATE", "gascity/builder")

	var stdout, stderr bytes.Buffer
	path1, err := doWorktreeHQ(cityDir, "ga-34q3ss", &stdout, &stderr)
	if err != nil {
		t.Fatalf("doWorktreeHQ() first call error = %v", err)
	}
	path2, err := doWorktreeHQ(cityDir, "ga-34q3ss", &stdout, &stderr)
	if err != nil {
		t.Fatalf("doWorktreeHQ() second call error = %v", err)
	}
	if path1 != path2 {
		t.Errorf("doWorktreeHQ() paths differ across calls: %q vs %q", path1, path2)
	}

	got := readRecordedArgs(t, recordFile)
	// Two invocations of 4 args each, recorded back-to-back.
	if len(got) != 8 {
		t.Fatalf("expected script to be invoked twice (8 recorded args), got %d: %q", len(got), got)
	}
}

func TestDoWorktreeHQPropagatesScriptFailure(t *testing.T) {
	cityDir := t.TempDir()
	recordFile := filepath.Join(t.TempDir(), "argv.txt")
	fakeSetupScript(t, cityDir, recordFile, 1)
	t.Setenv("GC_TEMPLATE", "gascity/builder")

	var stdout, stderr bytes.Buffer
	_, err := doWorktreeHQ(cityDir, "ga-34q3ss", &stdout, &stderr)
	if err == nil {
		t.Fatal("doWorktreeHQ() error = nil, want error on script failure")
	}

	worktreeDir := filepath.Join(cityDir, ".gc", "worktrees", "_hq", "builder-ga-34q3ss")
	if _, statErr := os.Stat(filepath.Join(worktreeDir, ".beads", "redirect")); !os.IsNotExist(statErr) {
		t.Errorf(".beads/redirect should not be written when the setup script fails")
	}
}

func TestDoWorktreeHQRejectsPathTraversalBeadID(t *testing.T) {
	cityDir := t.TempDir()
	recordFile := filepath.Join(t.TempDir(), "argv.txt")
	fakeSetupScript(t, cityDir, recordFile, 0)
	t.Setenv("GC_TEMPLATE", "gascity/builder")

	var stdout, stderr bytes.Buffer
	_, err := doWorktreeHQ(cityDir, "../../../escape", &stdout, &stderr)
	if err == nil {
		t.Fatal("doWorktreeHQ() error = nil, want error for a path-traversal bead ID")
	}

	if got := readRecordedArgs(t, recordFile); got != nil {
		t.Errorf("setup script should not be invoked for a path-traversal bead ID, got argv %q", got)
	}

	escaped := filepath.Join(cityDir, ".gc", "worktrees", "escape")
	if _, statErr := os.Stat(escaped); !os.IsNotExist(statErr) {
		t.Errorf("path-traversal bead ID must not create anything outside the HQ bucket, found %s", escaped)
	}
}

func TestDoWorktreeHQMissingBeadID(t *testing.T) {
	cityDir := t.TempDir()
	t.Setenv("GC_TEMPLATE", "gascity/builder")

	var stdout, stderr bytes.Buffer
	if _, err := doWorktreeHQ(cityDir, "   ", &stdout, &stderr); err == nil {
		t.Fatal("doWorktreeHQ() error = nil, want error for blank bead ID")
	}
}

func TestDoWorktreeHQMissingCallingRole(t *testing.T) {
	cityDir := t.TempDir()
	recordFile := filepath.Join(t.TempDir(), "argv.txt")
	fakeSetupScript(t, cityDir, recordFile, 0)
	t.Setenv("GC_TEMPLATE", "")
	t.Setenv("GC_AGENT", "")

	var stdout, stderr bytes.Buffer
	if _, err := doWorktreeHQ(cityDir, "ga-34q3ss", &stdout, &stderr); err == nil {
		t.Fatal("doWorktreeHQ() error = nil, want error when no calling role can be resolved")
	}
	if got := readRecordedArgs(t, recordFile); got != nil {
		t.Errorf("setup script should not be invoked when calling role is unresolved, got argv %q", got)
	}
}

func TestResolveCallingRolePrefersTemplateOverAgent(t *testing.T) {
	t.Setenv("GC_TEMPLATE", "gascity/builder")
	t.Setenv("GC_AGENT", "gascity/reviewer")

	if got := resolveCallingRole(); got != "builder" {
		t.Errorf("resolveCallingRole() = %q, want %q", got, "builder")
	}
}

func TestResolveCallingRoleFallsBackToAgent(t *testing.T) {
	t.Setenv("GC_TEMPLATE", "")
	t.Setenv("GC_AGENT", "gascity/reviewer")

	if got := resolveCallingRole(); got != "reviewer" {
		t.Errorf("resolveCallingRole() = %q, want %q", got, "reviewer")
	}
}

func TestResolveCallingRoleUnqualifiedIdentity(t *testing.T) {
	t.Setenv("GC_TEMPLATE", "")
	t.Setenv("GC_AGENT", "mayor")

	if got := resolveCallingRole(); got != "mayor" {
		t.Errorf("resolveCallingRole() = %q, want %q", got, "mayor")
	}
}

func TestResolveCallingRoleEmptyWhenUnset(t *testing.T) {
	t.Setenv("GC_TEMPLATE", "")
	t.Setenv("GC_AGENT", "")

	if got := resolveCallingRole(); got != "" {
		t.Errorf("resolveCallingRole() = %q, want empty", got)
	}
}
