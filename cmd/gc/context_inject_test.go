package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return p
}

func usageLine(model string, input, cacheRead, cacheCreate int) string {
	return fmt.Sprintf(
		`{"type":"assistant","message":{"model":%q,"usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`,
		model, input, cacheRead, cacheCreate)
}

func hookInputFor(path string) []byte {
	return []byte(fmt.Sprintf(`{"transcript_path":%q,"hook_event_name":"UserPromptSubmit"}`, path))
}

func TestContextInjectSilentBelowAdvisory(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// 100k of 1M = 10% — well below the 60% advisory threshold.
	p := writeTranscript(t, usageLine("claude-fable-5", 1_000, 98_000, 1_000))
	if got := contextInjectLine(hookInputFor(p)); got != "" {
		t.Errorf("below advisory should be silent, got %q", got)
	}
}

func TestContextInjectAdvisoryBand(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// 150k of 200k = 75% — the percentage advisory band. Exercised on a 200k
	// window because the absolute urgent floor (160k) sits below 60% of any
	// larger window, so on a 1M window the percentage advisory band is
	// subsumed by the absolute urgent tier (gcy-ty3, by design).
	p := writeTranscript(t, usageLine("some-other-model", 10_000, 130_000, 10_000))
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "150k/200k") || !strings.Contains(got, "~75%") {
		t.Errorf("advisory line wrong: %q", got)
	}
	if !strings.Contains(got, "clean seam") || !strings.Contains(got, "reset") {
		t.Errorf("advisory must point toward a clean seam + planned reset, got %q", got)
	}
	if strings.Contains(got, "HIGH") {
		t.Errorf("advisory band must not be marked HIGH: %q", got)
	}
}

func TestContextInjectUrgentBand(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// 900k of 1M = 90% — urgent band.
	p := writeTranscript(t, usageLine("claude-opus-4-8[1m]", 50_000, 800_000, 50_000))
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "HIGH") || !strings.Contains(got, "gc session reset") {
		t.Errorf("urgent line must direct to handoff + self gc session reset: %q", got)
	}
	if !strings.Contains(got, "operator") {
		t.Errorf("urgent line must preserve the operator-stay-up override: %q", got)
	}
}

func TestContextInjectLastUsageEntryWins(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// Older 90% entry followed by a newer 10% one (post-compaction shape):
	// the LAST entry is the live context size, so this must be silent.
	p := writeTranscript(t,
		usageLine("claude-fable-5", 50_000, 800_000, 50_000),
		usageLine("claude-fable-5", 5_000, 90_000, 5_000),
	)
	if got := contextInjectLine(hookInputFor(p)); got != "" {
		t.Errorf("last entry (10%%) should win and be silent, got %q", got)
	}
}

func TestContextInjectDefaultWindow200k(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// 150k on an unrecognized model = 75% of the conservative 200k default.
	p := writeTranscript(t, usageLine("some-other-model", 10_000, 130_000, 10_000))
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "150k/200k") || !strings.Contains(got, "~75%") {
		t.Errorf("200k default window not applied: %q", got)
	}
}

func TestContextInjectWindowOverride(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_WINDOW_TOKENS", "500000")
	p := writeTranscript(t, usageLine("some-other-model", 10_000, 380_000, 10_000))
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "400k/500k") {
		t.Errorf("window override not applied: %q", got)
	}
}

func TestContextInjectThresholdOverrides(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_ADVISORY_PCT", "30")
	t.Setenv("GC_CONTEXT_URGENT_PCT", "40")
	// 50% of 1M: above the overridden urgent threshold.
	p := writeTranscript(t, usageLine("claude-fable-5", 10_000, 480_000, 10_000))
	if got := contextInjectLine(hookInputFor(p)); !strings.Contains(got, "HIGH") {
		t.Errorf("threshold overrides not applied: %q", got)
	}
}

func TestContextInjectDisabled(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "0")
	p := writeTranscript(t, usageLine("claude-fable-5", 50_000, 800_000, 50_000))
	if got := contextInjectLine(hookInputFor(p)); got != "" {
		t.Errorf("disabled should be silent, got %q", got)
	}
}

func TestContextInjectFailSafeSilent(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	for name, input := range map[string][]byte{
		"nil stdin":          nil,
		"garbage stdin":      []byte("not json"),
		"no transcript path": []byte(`{"hook_event_name":"UserPromptSubmit"}`),
		"missing file":       hookInputFor("/nonexistent/transcript.jsonl"),
	} {
		if got := contextInjectLine(input); got != "" {
			t.Errorf("%s: want silent, got %q", name, got)
		}
	}
	// Transcript with no usage entries.
	p := writeTranscript(t, `{"type":"user","message":{"content":"hi"}}`)
	if got := contextInjectLine(hookInputFor(p)); got != "" {
		t.Errorf("no-usage transcript: want silent, got %q", got)
	}
}

// Regression: the newest usage entry lacking a model string must not flip a
// 1M session to the 200k default (would fire the urgent tier far too early).
func TestContextInjectLastNonEmptyModelWins(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// First entry names the 1M model; the newest usage entry omits model.
	// 700k must read as 70% of 1M (advisory), not 350% of 200k.
	p := writeTranscript(t,
		usageLine("claude-fable-5", 10_000, 680_000, 10_000),
		`{"type":"assistant","message":{"usage":{"input_tokens":10000,"cache_read_input_tokens":680000,"cache_creation_input_tokens":10000}}}`,
	)
	got := contextInjectLine(hookInputFor(p))
	// The rendered denominator alone proves the window survived (700k/1000k vs
	// 700k/200k). Tier is no longer a proxy for it: since gcy-ty3, 700k trips
	// the absolute urgent floor on either window.
	if !strings.Contains(got, "700k/1000k") {
		t.Errorf("empty-model newest entry must retain the 1M window: %q", got)
	}
}

// Bare claude-opus-4-8 is a 1M-context model (no [1m] suffix in the transcript).
func TestContextInjectBareOpus48Is1M(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	p := writeTranscript(t, usageLine("claude-opus-4-8", 10_000, 680_000, 10_000))
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "700k/1000k") {
		t.Errorf("bare opus-4-8 must resolve to the 1M window: %q", got)
	}
}

// Sidecar/compaction call on a smaller-window model must not shrink the
// main-loop session's window: max-over-models wins. (The observed 782k/200k
// bug: a Fable session with bare-opus sidecar entries, newest entry opus.)
func TestContextInjectSidecarDoesNotShrinkWindow(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// Newest entry classifies 200k but carries the live (high) token count; an
	// earlier entry is the 1M main-loop model. Window must be 1M (max), so 700k
	// reads as ~70% (advisory), not ~350% of 200k.
	p := writeTranscript(t,
		usageLine("claude-fable-5", 10_000, 680_000, 10_000),   // main loop, 1M
		usageLine("claude-haiku-4-5", 10_000, 680_000, 10_000), // 200k-classified, newest, high tokens
	)
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "700k/1000k") {
		t.Errorf("a 200k-classified newest entry must not shrink the 1M session window: %q", got)
	}
}

// --- gcy-ty3: absolute-token companions to the percentage gate ---------------
//
// The percentage gate alone is window-relative, so a 1M-window agent was not
// advised until 600k and not urged until 800k. These cover the absolute tier.

// The measured defect (sysadmin refinery transcript e60663db): 500k of a 1M
// window reads as 50% — below the 60% advisory — so the session ran 18.7h
// without a single nudge. It must now trip the urgent tier on depth alone.
func TestContextInjectMeasuredRefineryPeakIsUrgent(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	p := writeTranscript(t, usageLine("claude-opus-4-8[1m]", 10_000, 480_000, 10_000))
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "HIGH") {
		t.Errorf("500k of 1M (50%%) must trip urgent on absolute depth, got %q", got)
	}
	if !strings.Contains(got, "500k/1000k") {
		t.Errorf("expected 500k/1000k rendering, got %q", got)
	}
}

// 150k of 1M is 15% — far below the 60% advisory — but past the 120k absolute
// advisory. Must advise, and must NOT be marked urgent (150k <= 160k).
func TestContextInject1MAdvisoryOnAbsoluteTokens(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	p := writeTranscript(t, usageLine("claude-fable-5", 10_000, 130_000, 10_000))
	got := contextInjectLine(hookInputFor(p))
	if got == "" {
		t.Fatal("150k of 1M must trip the absolute advisory, got silence")
	}
	if strings.Contains(got, "HIGH") {
		t.Errorf("150k is below the 160k urgent floor, must stay advisory: %q", got)
	}
	if !strings.Contains(got, "150k/1000k") || !strings.Contains(got, "clean seam") {
		t.Errorf("advisory rendering wrong: %q", got)
	}
}

// 700k of 1M is 70% — inside the percentage advisory band — but 4x past the
// absolute urgent floor. Whichever trips FIRST wins, so this is urgent.
func TestContextInject1MUrgentOnAbsoluteTokens(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	p := writeTranscript(t, usageLine("claude-fable-5", 10_000, 680_000, 10_000))
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "HIGH") || !strings.Contains(got, "gc session reset") {
		t.Errorf("700k must trip urgent on absolute depth: %q", got)
	}
}

// Non-regression guard (passes before AND after the fix): on a 200k window the
// default absolute floors (120k/160k) coincide exactly with the default 60%/80%
// gates, so the added conjuncts are redundant and every tier boundary — including
// the inclusive-advisory / exclusive-urgent edges — is bit-for-bit unchanged.
func TestContextInject200kWindowTiersUnchanged(t *testing.T) {
	for _, env := range []string{
		"GC_CONTEXT_ADVISORY_PCT", "GC_CONTEXT_URGENT_PCT",
		"GC_CONTEXT_ADVISORY_TOKENS", "GC_CONTEXT_URGENT_TOKENS",
	} {
		t.Setenv(env, "")
	}
	for _, tc := range []struct {
		tokens int
		want   string
	}{
		{119_999, "silent"},
		{120_000, "advisory"},
		{160_000, "advisory"},
		{160_001, "urgent"},
	} {
		got := contextUsageMessage(tc.tokens, 200_000)
		var tier string
		switch {
		case got == "":
			tier = "silent"
		case strings.Contains(got, "HIGH"):
			tier = "urgent"
		default:
			tier = "advisory"
		}
		if tier != tc.want {
			t.Errorf("200k window, %d tokens: got %s, want %s", tc.tokens, tier, tc.want)
		}
	}
}

// The absolute knobs are env-overridable, and raising them makes a deep session
// quieter — the escape hatch for operators who deliberately run long 1M sessions.
func TestContextInjectAbsoluteThresholdOverrides(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_ADVISORY_TOKENS", "300000")
	t.Setenv("GC_CONTEXT_URGENT_TOKENS", "400000")
	// 250k of 1M: 25% (below the 60% pct gate) and below the raised 300k floor.
	p := writeTranscript(t, usageLine("claude-fable-5", 10_000, 230_000, 10_000))
	if got := contextInjectLine(hookInputFor(p)); got != "" {
		t.Errorf("raised absolute floors must silence 250k, got %q", got)
	}
	// 350k: past the raised advisory floor, below the raised urgent floor.
	p = writeTranscript(t, usageLine("claude-fable-5", 10_000, 330_000, 10_000))
	got := contextInjectLine(hookInputFor(p))
	if got == "" || strings.Contains(got, "HIGH") {
		t.Errorf("350k must be advisory under raised floors, got %q", got)
	}
}

// The two knob families need separate parsers: thresholdPct's 1..100 clamp must
// not silently swallow a token count typed into the percentage variable, and
// thresholdTokens must accept values >100 that thresholdPct would reject.
func TestContextInjectPctAndTokenKnobsParseSeparately(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// Operator meant tokens but typed it into the _PCT knob. thresholdPct
	// rejects >100 and falls back to 60 — the gate must NOT be disabled, and
	// the absolute companion must still fire at its own default.
	t.Setenv("GC_CONTEXT_ADVISORY_PCT", "120000")
	p := writeTranscript(t, usageLine("claude-fable-5", 10_000, 120_000, 10_000))
	if got := contextInjectLine(hookInputFor(p)); !strings.Contains(got, "140k/1000k") {
		t.Errorf("out-of-range _PCT must fall back to its default, not disable the gate: %q", got)
	}
	// Same numeric value in the _TOKENS knob IS honored (thresholdPct would
	// have rejected it), proving the parsers are distinct.
	t.Setenv("GC_CONTEXT_ADVISORY_PCT", "")
	t.Setenv("GC_CONTEXT_ADVISORY_TOKENS", "120000")
	if got := contextUsageMessage(119_999, 1_000_000); got != "" {
		t.Errorf("1 token below the honored absolute floor must be silent: %q", got)
	}
	if got := contextUsageMessage(120_000, 1_000_000); got == "" {
		t.Error("120000 in the _TOKENS knob must be honored as an absolute floor")
	}
}
