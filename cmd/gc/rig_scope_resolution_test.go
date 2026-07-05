package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestRigFromRedirectedBeadsDirIgnoresCwdOutsideCity verifies that when the
// caller's cwd is outside cityPath, any .beads/redirect found while walking
// the cwd's ancestor chain is ignored. The walk must be bounded by cityPath
// so that a polecat worktree's foreign-rig redirect (e.g., the shared rig
// repo checkout at /home/b/GIT/gascity/.beads) cannot bleed into rig
// resolution against an unrelated city.
func TestRigFromRedirectedBeadsDirIgnoresCwdOutsideCity(t *testing.T) {
	foreignRoot := filepath.Join(t.TempDir(), "foreign")
	if err := os.MkdirAll(filepath.Join(foreignRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwdRoot := t.TempDir()
	cwd := filepath.Join(cwdRoot, "worktree", "polecat-1")
	if err := os.MkdirAll(filepath.Join(cwd, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cwd, ".beads", "redirect"),
		[]byte(filepath.Join(foreignRoot, ".beads")+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "frontend", Path: filepath.Join("rigs", "frontend"), Prefix: "fr"},
		},
	}

	rig, ok, err := rigFromRedirectedBeadsDir(cfg, cityDir, normalizePathForCompare(cwd))
	if err != nil {
		t.Fatalf("rigFromRedirectedBeadsDir() error = %v, want nil (cwd outside cityPath)", err)
	}
	if ok {
		t.Fatalf("rigFromRedirectedBeadsDir() ok = true, want false; rig = %+v", rig)
	}
}

// TestRigFromRedirectedBeadsDirCityBeadsRedirectIsNotAnError verifies that a
// .beads/redirect pointing at the city's own top-level .beads dir (as written
// by "gc worktree hq" for HQ-targeting bead worktrees) resolves to "no rig"
// without an error, rather than tripping the "points outside declared city
// rigs" error meant for genuinely unexpected redirect targets.
func TestRigFromRedirectedBeadsDirCityBeadsRedirectIsNotAnError(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	hqWorktree := filepath.Join(cityDir, ".gc", "worktrees", "_hq", "builder-ga-99999")
	if err := os.MkdirAll(filepath.Join(hqWorktree, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(hqWorktree, ".beads", "redirect"),
		[]byte(filepath.Join(cityDir, ".beads")+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "frontend", Path: filepath.Join("rigs", "frontend"), Prefix: "fr"},
		},
	}

	rig, ok, err := rigFromRedirectedBeadsDir(cfg, cityDir, normalizePathForCompare(hqWorktree))
	if err != nil {
		t.Fatalf("rigFromRedirectedBeadsDir() error = %v, want nil (redirect targets the city's own .beads)", err)
	}
	if ok {
		t.Fatalf("rigFromRedirectedBeadsDir() ok = true, want false; rig = %+v", rig)
	}
}

// TestRigFromRedirectedBeadsDirMismatchedRedirectStillErrors locks in the
// pre-existing safety net: a .beads/redirect pointing somewhere that is
// neither a declared rig's .beads dir nor the city's own .beads dir is a
// genuine misconfiguration and must still surface as an error.
func TestRigFromRedirectedBeadsDirMismatchedRedirectStillErrors(t *testing.T) {
	cityDir := t.TempDir()
	bogusTarget := filepath.Join(t.TempDir(), ".beads")
	worktree := filepath.Join(cityDir, "somewhere", "else")
	if err := os.MkdirAll(filepath.Join(worktree, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(worktree, ".beads", "redirect"),
		[]byte(bogusTarget+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "frontend", Path: filepath.Join("rigs", "frontend"), Prefix: "fr"},
		},
	}

	_, ok, err := rigFromRedirectedBeadsDir(cfg, cityDir, normalizePathForCompare(worktree))
	if err == nil {
		t.Fatalf("rigFromRedirectedBeadsDir() error = nil, want error for mismatched redirect target")
	}
	if ok {
		t.Fatalf("rigFromRedirectedBeadsDir() ok = true, want false alongside error")
	}
}
