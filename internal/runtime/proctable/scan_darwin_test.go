//go:build darwin

package proctable

import "testing"

// psFields mimics the whitespace-split line psRecords builds a record from:
// "<pid> <ppid> <command...>" with `ps eww` appending envp to argv with no
// delimiter between them.
func psFields(command ...string) []string {
	return command
}

// The tmux server is spawned as
//
//	tmux -u -L gc new-session -d -s s-<id> -e GC_SESSION_ID=<id> -e ...
//
// so its argv carries the `-e KEY=VALUE` flags used to seed the session
// environment. psRecords cannot separate argv from envp in `ps eww` output, so
// those argv tokens are parsed as if they were the server's own environment and
// the server impersonates the session it spawned. It must still never be
// reported as an agent root: killExistingOrphans terminates untracked roots, and
// killing the runtime server takes every session in the city down with it.
func TestRootsFromRecordsSkipsRuntimeServer(t *testing.T) {
	records := map[int]psRecord{
		1822: {
			pid:     1822,
			ppid:    1, // daemonized: reparented to launchd
			command: "tmux",
			env: parseInlineEnv(psFields(
				"-u", "-L", "gc", "new-session", "-d", "-s", "s-gc-wisp-q0mv",
				"-e", "GC_SESSION_ID=gc-wisp-q0mv",
				"-e", "GC_CITY_PATH=/Users/jonesy/Developer/gc",
				"-e", "GC_RUNTIME_EPOCH=13",
			)),
		},
	}

	got := rootsFromRecords(records, "gc-wisp-q0mv")

	if len(got) != 0 {
		t.Fatalf("tmux server reported as an agent root for gc-wisp-q0mv: %+v\n"+
			"the runtime server is not an agent process; returning it here makes it a kill target "+
			"for killExistingOrphans, which takes down every session on that server", got)
	}
}

// Guard against over-fixing: pane leaders legitimately sit directly under the
// tmux server, and they are the roots the scan exists to find.
func TestRootsFromRecordsKeepsPaneRootUnderServer(t *testing.T) {
	records := map[int]psRecord{
		1822: {
			pid:     1822,
			ppid:    1,
			command: "tmux",
			env:     parseInlineEnv(psFields("-e", "GC_SESSION_ID=gc-wisp-q0mv")),
		},
		2825: {
			pid:     2825,
			ppid:    1822,
			command: "claude",
			env: map[string]string{
				"GC_SESSION_ID":    "gc-8lye",
				"GC_CITY_PATH":     "/Users/jonesy/Developer/gc",
				"GC_RUNTIME_EPOCH": "13",
			},
		},
	}

	got := rootsFromRecords(records, "gc-8lye")

	if len(got) != 1 {
		t.Fatalf("want exactly the pane leader as root, got %+v", got)
	}
	if got[0].PID != 2825 {
		t.Fatalf("want pane leader pid 2825, got %d", got[0].PID)
	}
	if got[0].City != "/Users/jonesy/Developer/gc" {
		t.Fatalf("want city from GC_CITY_PATH, got %q", got[0].City)
	}
	if got[0].Epoch != 13 {
		t.Fatalf("want epoch 13, got %d", got[0].Epoch)
	}
}

// Documents the underlying hazard the guard compensates for: any KEY=VALUE
// token in argv is indistinguishable from a real environment entry in
// `ps eww` output. This is why the fix cannot rely on the env map alone.
func TestParseInlineEnvCannotDistinguishArgvFromEnviron(t *testing.T) {
	env := parseInlineEnv(psFields("-e", "GC_SESSION_ID=from-argv"))

	if env["GC_SESSION_ID"] != "from-argv" {
		t.Fatalf("expected argv assignment to be parsed as env (documenting the hazard), got %q",
			env["GC_SESSION_ID"])
	}
}
