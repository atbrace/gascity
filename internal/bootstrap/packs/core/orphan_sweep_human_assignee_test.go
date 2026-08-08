package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGC writes a `gc` stub that serves orphan-sweep.sh the minimum JSON it
// needs to reach a release decision, and records every `bd release-if-current`
// it is asked to perform. inProgress is the JSON array returned for
// `gc bd list --status=in_progress`; every `gc bd show` returns the single
// bead so the re-read guard passes and the sweep proceeds to the decision.
func fakeGC(t *testing.T, binDir, inProgress, beadShow string) string {
	t.Helper()
	releaseLog := filepath.Join(binDir, "released.log")
	script := `#!/bin/sh
release_log=` + shq(releaseLog) + `
case "$*" in
  *"release-if-current"*)
    printf '%s\n' "$*" >> "$release_log"
    echo "released"
    exit 0 ;;
  "rig list --json")
    echo '{"rigs":[]}'; exit 0 ;;
  "session list --json")
    echo '{"sessions":[]}'; exit 0 ;;
  *"bd list"*)
    cat <<'JSON'
` + inProgress + `
JSON
    exit 0 ;;
  *"bd show"*)
    cat <<'JSON'
` + beadShow + `
JSON
    exit 0 ;;
  "config explain")
    echo "Agent: mayor"; exit 0 ;;
esac
exit 0
`
	path := filepath.Join(binDir, "gc")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake gc: %v", err)
	}
	return releaseLog
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func runOrphanSweep(t *testing.T, inProgress, beadShow string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}
	binDir := t.TempDir()
	releaseLog := fakeGC(t, binDir, inProgress, beadShow)

	cmd := exec.Command("bash", filepath.Join("assets", "scripts", "orphan-sweep.sh"))
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("orphan-sweep.sh failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(releaseLog)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("reading release log: %v", err)
	}
	return string(data)
}

// An interactive `bd update --claim` stamps the caller's git identity, so real
// beads carry human display-name assignees. Those name no agent and no session,
// so the sweep used to classify them as dead-agent work and strip the
// operator's claim every cycle.
func TestOrphanSweep_LeavesHumanDisplayNameAssigneeAlone(t *testing.T) {
	const bead = `[{"id":"sys-j7m0t","status":"in_progress","assignee":"Austin Brace"}]`
	if released := runOrphanSweep(t, bead, bead); released != "" {
		t.Fatalf("sweep released a human-assigned bead: %s", released)
	}
}

func TestOrphanSweep_LeavesCanonicalHumanAliasAlone(t *testing.T) {
	const bead = `[{"id":"sys-aaaaa","status":"in_progress","assignee":"human"}]`
	if released := runOrphanSweep(t, bead, bead); released != "" {
		t.Fatalf("sweep released a human-alias bead: %s", released)
	}
}

// The guard must stay narrow: a genuinely dead agent (a token-shaped name with
// no config entry and no live session) is still swept.
func TestOrphanSweep_StillReleasesDeadAgentWork(t *testing.T) {
	const bead = `[{"id":"sys-bbbbb","status":"in_progress","assignee":"gastown__polecat-gc-dead1"}]`
	released := runOrphanSweep(t, bead, bead)
	if !strings.Contains(released, "sys-bbbbb") {
		t.Fatalf("sweep did not release dead-agent work; log = %q", released)
	}
}
