package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func gaConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
	}
}

func TestExtractBeadIDFromWorktreeNameBareID(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "ga-n0oafq")
	if got != "ga-n0oafq" {
		t.Errorf("got %q, want %q", got, "ga-n0oafq")
	}
}

func TestExtractBeadIDFromWorktreeNameCompound(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "builder-ga-34q3ss")
	if got != "ga-34q3ss" {
		t.Errorf("got %q, want %q", got, "ga-34q3ss")
	}
}

func TestExtractBeadIDFromWorktreeNameNoMatch(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "builder-feature-branch")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractBeadIDFromWorktreeNameSingleSegment(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "builder")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractBeadIDFromWorktreeNameNilConfig(t *testing.T) {
	got := extractBeadIDFromWorktreeName(nil, "ga-n0oafq")
	if got != "" {
		t.Errorf("got %q, want empty for nil config", got)
	}
}

func TestExtractBeadIDFromWorktreeNameEmptyName(t *testing.T) {
	got := extractBeadIDFromWorktreeName(gaConfig(), "")
	if got != "" {
		t.Errorf("got %q, want empty for empty name", got)
	}
}

func TestIsStrictlyUnderDirSubpath(t *testing.T) {
	dir := filepath.Join("a", "b")
	path := filepath.Join("a", "b", "c")
	if !isStrictlyUnderDir(dir, path) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = false, want true", dir, path)
	}
}

func TestIsStrictlyUnderDirSameDir(t *testing.T) {
	dir := filepath.Join("a", "b")
	if isStrictlyUnderDir(dir, dir) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = true, want false (same dir)", dir, dir)
	}
}

func TestIsStrictlyUnderDirPathTraversal(t *testing.T) {
	dir := filepath.Join("a", "b")
	path := filepath.Join("a", "c") // sibling — relative path starts with ".."
	if isStrictlyUnderDir(dir, path) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = true, want false (path traversal)", dir, path)
	}
}

func TestIsStrictlyUnderDirDeepSubpath(t *testing.T) {
	dir := filepath.Join("root", "worktrees")
	path := filepath.Join("root", "worktrees", "gascity", "builder")
	if !isStrictlyUnderDir(dir, path) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = false, want true", dir, path)
	}
}

// fixedStatusStore is a minimal beads.Store fake that reports a fixed status
// for exactly one bead ID and errors on anything else. Non-Get methods are
// promoted from the embedded unavailableStore.
type fixedStatusStore struct {
	unavailableStore
	beadID string
	status string
}

func (s fixedStatusStore) Get(id string) (beads.Bead, error) {
	if id != s.beadID {
		return beads.Bead{}, errors.New("bead not found")
	}
	return beads.Bead{ID: id, Status: s.status}, nil
}

var _ beads.Store = fixedStatusStore{}

func TestWithHQBeadStoreAddsHQKeyWithoutMutatingInput(t *testing.T) {
	rig := unavailableStore{err: errors.New("rig")}
	hq := unavailableStore{err: errors.New("hq")}
	original := map[string]beads.Store{"gascity": rig}

	merged := withHQBeadStore(original, hq)

	if len(original) != 1 {
		t.Fatalf("input map mutated: len=%d, want 1", len(original))
	}
	if _, ok := original[hqBeadWorktreeBucket]; ok {
		t.Fatal("input map mutated: _hq key leaked into original map")
	}
	if merged["gascity"] != rig {
		t.Error("merged map missing/altered original rig entry")
	}
	if merged[hqBeadWorktreeBucket] != hq {
		t.Error("merged map missing _hq entry")
	}
	if len(merged) != 2 {
		t.Fatalf("len(merged) = %d, want 2", len(merged))
	}
}

func TestWithHQBeadStoreNilHQStoreOmitsKey(t *testing.T) {
	merged := withHQBeadStore(map[string]beads.Store{"gascity": unavailableStore{}}, nil)
	if _, ok := merged[hqBeadWorktreeBucket]; ok {
		t.Error("expected no _hq key when hqStore is nil")
	}
	if len(merged) != 1 {
		t.Fatalf("len(merged) = %d, want 1", len(merged))
	}
}

func TestWithHQBeadStoreNilInputMap(t *testing.T) {
	hq := unavailableStore{err: errors.New("hq")}
	merged := withHQBeadStore(nil, hq)
	if merged[hqBeadWorktreeBucket] != hq {
		t.Error("expected _hq entry even when input map is nil")
	}
	if len(merged) != 1 {
		t.Fatalf("len(merged) = %d, want 1", len(merged))
	}
}

// makeHQCityRepo creates a real git repo (with an "origin" remote already
// holding the initial commit) suitable for exercising reapClosedBeadWorktrees'
// git-backed safety gates end to end.
func makeHQCityRepo(t *testing.T) string {
	t.Helper()
	bare := t.TempDir()
	runGitInTest(t, bare, "init", "--bare")

	cityPath := filepath.Join(t.TempDir(), "hq-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitInTest(t, cityPath, "init")
	runGitInTest(t, cityPath, "config", "user.email", "test@test.com")
	runGitInTest(t, cityPath, "config", "user.name", "Test")
	runGitInTest(t, cityPath, "checkout", "-b", "main")
	runGitInTest(t, cityPath, "commit", "--allow-empty", "-m", "init")
	runGitInTest(t, cityPath, "remote", "add", "origin", bare)
	runGitInTest(t, cityPath, "push", "-u", "origin", "main")
	return cityPath
}

func TestReapClosedBeadWorktreesCoversHQBucket(t *testing.T) {
	cityPath := makeHQCityRepo(t)
	cfg := gaConfig()

	hqWorktreeDir := filepath.Join(cityPath, ".gc", "worktrees", hqBeadWorktreeBucket, "architect-ga-gr9pm9.1")
	runGitInTest(t, cityPath, "worktree", "add", "-b", "wt-ga-gr9pm9-1", hqWorktreeDir)

	stores := map[string]beads.Store{
		hqBeadWorktreeBucket: fixedStatusStore{beadID: "ga-gr9pm9.1", status: "closed"},
	}

	reaped := reapClosedBeadWorktrees(cityPath, cfg, stores, nil, io.Discard)

	if reaped != 1 {
		t.Fatalf("reapClosedBeadWorktrees reaped = %d, want 1", reaped)
	}
	if _, err := os.Stat(hqWorktreeDir); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed, stat err = %v", hqWorktreeDir, err)
	}
}

func TestReapClosedBeadWorktreesSkipsOpenHQBead(t *testing.T) {
	cityPath := makeHQCityRepo(t)
	cfg := gaConfig()

	hqWorktreeDir := filepath.Join(cityPath, ".gc", "worktrees", hqBeadWorktreeBucket, "architect-ga-gr9pm9.1")
	runGitInTest(t, cityPath, "worktree", "add", "-b", "wt-ga-gr9pm9-1-open", hqWorktreeDir)

	stores := map[string]beads.Store{
		hqBeadWorktreeBucket: fixedStatusStore{beadID: "ga-gr9pm9.1", status: "open"},
	}

	reaped := reapClosedBeadWorktrees(cityPath, cfg, stores, nil, io.Discard)

	if reaped != 0 {
		t.Fatalf("reapClosedBeadWorktrees reaped = %d, want 0 for an open bead", reaped)
	}
	if _, err := os.Stat(hqWorktreeDir); err != nil {
		t.Fatalf("expected %s to survive, stat err = %v", hqWorktreeDir, err)
	}
}
