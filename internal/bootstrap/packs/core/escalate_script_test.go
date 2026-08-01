package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runEscalate executes assets/scripts/escalate.sh with a fake `gc` on PATH
// that appends its argv to a log file. It returns the script's combined
// output and everything the fake `gc` was asked to do.
func runEscalate(t *testing.T, env []string, args ...string) (output, gcLog string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "gc.log")
	fakeGC := "#!/bin/sh\nprintf 'gc %s\\n' \"$*\" >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gc"), []byte(fakeGC), 0o755); err != nil {
		t.Fatalf("write fake gc: %v", err)
	}

	cmd := exec.Command("bash", filepath.Join("assets", "scripts", "escalate.sh"))
	cmd.Args = append(cmd.Args, args...)
	cmd.Env = append([]string{"PATH=" + binDir + ":" + os.Getenv("PATH")}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("escalate.sh %v failed: %v\n%s", args, err, out)
	}

	logged, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read gc log: %v", readErr)
	}
	return string(out), string(logged)
}

// TestEscalatePagesHumanForOutageSeverities asserts CRITICAL and HIGH keep
// reaching the reserved human mailbox — a person should see an outage.
func TestEscalatePagesHumanForOutageSeverities(t *testing.T) {
	for _, severity := range []string{"CRITICAL", "HIGH", "critical", "high"} {
		t.Run(severity, func(t *testing.T) {
			_, gcLog := runEscalate(t, nil,
				"--subject", "Dolt server unreachable",
				"--message", "probe failed",
				"--severity", severity)
			want := "gc mail send human -s Dolt server unreachable [" + severity + "]"
			if !strings.Contains(gcLog, want) {
				t.Fatalf("severity %s must page the human mailbox\nwant: %s\ngot log:\n%s", severity, want, gcLog)
			}
		})
	}
}

// TestEscalateDoesNotPageHumanForTriageSeverities is the regression guard for
// gcy-pq2: MEDIUM/low/unset advisories poisoned the human mailbox (84 of 89
// unread were "Dolt health advisory [MEDIUM]"). With no triage mailbox
// configured they must not be mailed at all.
func TestEscalateDoesNotPageHumanForTriageSeverities(t *testing.T) {
	for _, severity := range []string{"MEDIUM", "medium", "low", ""} {
		name := severity
		if name == "" {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			args := []string{"--subject", "Dolt health advisory", "--message", "latency 1200ms"}
			if severity != "" {
				args = append(args, "--severity", severity)
			}
			out, gcLog := runEscalate(t, nil, args...)
			if strings.Contains(gcLog, "mail send") {
				t.Fatalf("severity %q must not mail when no triage recipient is configured, log:\n%s", severity, gcLog)
			}
			if !strings.Contains(out, "GC_ESCALATION_TRIAGE_RECIPIENT") {
				t.Fatalf("unrouted advisory must name the env var that routes it, output:\n%s", out)
			}
			if !strings.Contains(out, "Dolt health advisory") || !strings.Contains(out, "latency 1200ms") {
				t.Fatalf("unrouted advisory must still report subject and body, output:\n%s", out)
			}
		})
	}
}

// TestEscalateRoutesTriageToConfiguredMailbox asserts a city that wants
// advisories triaged by an agent gets them there instead of at the human.
func TestEscalateRoutesTriageToConfiguredMailbox(t *testing.T) {
	_, gcLog := runEscalate(t, []string{"GC_ESCALATION_TRIAGE_RECIPIENT=triage-agent"},
		"--subject", "Dolt health advisory",
		"--message", "latency 1200ms",
		"--severity", "MEDIUM")
	if !strings.Contains(gcLog, "gc mail send triage-agent -s Dolt health advisory [MEDIUM]") {
		t.Fatalf("triage severity must route to the configured mailbox, log:\n%s", gcLog)
	}
}

// TestEscalatePageRecipientIsConfigurable asserts the page tier is not
// hardwired to `human` either — deployments can point it at an oncall alias.
func TestEscalatePageRecipientIsConfigurable(t *testing.T) {
	_, gcLog := runEscalate(t, []string{"GC_ESCALATION_PAGE_RECIPIENT=oncall"},
		"--subject", "Dolt server unreachable",
		"--message", "probe failed",
		"--severity", "CRITICAL")
	if !strings.Contains(gcLog, "gc mail send oncall -s Dolt server unreachable [CRITICAL]") {
		t.Fatalf("page tier must honor GC_ESCALATION_PAGE_RECIPIENT, log:\n%s", gcLog)
	}
}

// TestEscalationRecipientOverridesEverySeverity preserves the documented
// behavior of the pre-existing knob: it wins at every severity.
func TestEscalationRecipientOverridesEverySeverity(t *testing.T) {
	for _, severity := range []string{"CRITICAL", "MEDIUM"} {
		t.Run(severity, func(t *testing.T) {
			_, gcLog := runEscalate(t,
				[]string{
					"GC_ESCALATION_RECIPIENT=ops",
					"GC_ESCALATION_PAGE_RECIPIENT=oncall",
					"GC_ESCALATION_TRIAGE_RECIPIENT=triage-agent",
				},
				"--subject", "Dolt health advisory",
				"--message", "body",
				"--severity", severity)
			if !strings.Contains(gcLog, "gc mail send ops -s") {
				t.Fatalf("GC_ESCALATION_RECIPIENT must override severity routing at %s, log:\n%s", severity, gcLog)
			}
		})
	}
}

// TestEscalatePreservesExplicitSubjectSuffix asserts a subject that already
// carries its own bracketed tag is not double-tagged.
func TestEscalatePreservesExplicitSubjectSuffix(t *testing.T) {
	_, gcLog := runEscalate(t, []string{"GC_ESCALATION_RECIPIENT=ops"},
		"--subject", "Unservable Dolt databases detected [HIGH]",
		"--message", "body",
		"--severity", "HIGH")
	if strings.Contains(gcLog, "[HIGH] [HIGH]") {
		t.Fatalf("subject must not be double-tagged, log:\n%s", gcLog)
	}
}
