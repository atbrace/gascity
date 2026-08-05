package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/modelwindow"
)

// Context-usage injection — the context-pressure sibling of clock_inject.go.
//
// Gas City has canonical handoff machinery (`gc handoff`, the PreCompact
// auto-handoff, deployment handoff skills) but agents have no signal for WHEN
// to trigger it: a session cannot see its own context usage (the provider
// footer is rendered for humans only), so unmonitored agents run into context
// compaction by default — losing the deliberate wrap-up (durable notes, bead
// updates, clean seams) the handoff machinery exists to provide.
//
// This reads the provider hook input (UserPromptSubmit JSON on stdin carries
// transcript_path), computes the session's current context footprint from the
// last usage entry in the transcript, and injects ONE line of guidance —
// folded into the same single provider payload as the clock (see
// cmd_nudge.go), so JSON hook formats stay one valid document.
//
// THRESHOLD-GATED BY DESIGN — not an always-on countdown. Model-provider
// guidance (Anthropic, Claude Fable 5 migration notes) documents "context
// anxiety": a continuously visible remaining-context count induces premature
// wrap-up and unprompted session-splitting. Below the advisory threshold this
// injects NOTHING. Above it, the message is actionable ("steer toward a clean
// handoff point", "run your handoff process now") and explicitly tells the
// agent NOT to panic-stop at the advisory tier.
//
//	< advisory (default 60%)  : silent
//	advisory..urgent (60–80%) : plan toward a clean handoff point
//	> urgent (default 80%)    : trigger the canonical handoff now
//
// Knobs: GC_INJECT_CONTEXT=0|false|off disables; GC_CONTEXT_ADVISORY_PCT and
// GC_CONTEXT_URGENT_PCT override the thresholds; GC_CONTEXT_WINDOW_TOKENS
// overrides the context-window size when model-string detection is wrong.
// Fail-safe: any parse/read problem returns "" — never blocks a prompt.

// hookStdinInput is the subset of the provider hook JSON we need.
type hookStdinInput struct {
	TranscriptPath string `json:"transcript_path"`
}

// transcriptUsage is the usage block shape inside provider transcript entries.
type transcriptUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// contextInjectLine returns the context-usage guidance line for the session
// whose hook input JSON is in hookInput, or "" when disabled, below the
// advisory threshold, or on any error (fail-safe silent).
func contextInjectLine(hookInput []byte) string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GC_INJECT_CONTEXT"))) {
	case "0", "false", "off":
		return ""
	}
	var in hookStdinInput
	if err := json.Unmarshal(hookInput, &in); err != nil || strings.TrimSpace(in.TranscriptPath) == "" {
		return ""
	}
	tokens, models, ok := lastTranscriptUsage(in.TranscriptPath)
	if !ok {
		return ""
	}
	return contextUsageMessage(tokens, contextWindowTokens(models))
}

// lastTranscriptUsage reads the tail of a provider transcript (JSONL) and
// returns the context footprint of the most recent usage entry (prompt-side
// input tokens + cache reads + cache writes ≈ current context size) plus every
// non-empty model string seen — the window is the MAX over those (see
// contextWindowTokens), so a smaller-window sidecar/compaction call logged in
// the same transcript can't shrink the main-loop session's window.
func lastTranscriptUsage(path string) (tokens int, models []string, ok bool) {
	const tailBytes = 2 << 20 // last 2MiB is ample for the newest entries
	f, err := os.Open(path)   //nolint:gosec // path comes from the provider hook input
	if err != nil {
		return 0, nil, false
	}
	defer f.Close() //nolint:errcheck // read-only
	if st, err := f.Stat(); err == nil && st.Size() > tailBytes {
		if _, err := f.Seek(st.Size()-tailBytes, io.SeekStart); err != nil {
			return 0, nil, false
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, tailBytes))
	if err != nil {
		return 0, nil, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, `"usage"`) {
			continue
		}
		var entry struct {
			Message struct {
				Model string           `json:"model"`
				Usage *transcriptUsage `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Message.Usage == nil {
			continue
		}
		u := entry.Message.Usage
		if u.InputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
			continue
		}
		// Tokens: the LAST qualifying entry is the live context size (after a
		// compaction the newest entry reads low again).
		tokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		if m := entry.Message.Model; m != "" {
			models = append(models, m)
		}
		ok = true
	}
	return tokens, models, ok
}

// contextWindowTokens resolves the session's context window as the MAX window
// of any model it ran (they share one context), so a 200k-window sidecar or
// compaction call (e.g. a bare claude-opus-4-8 entry inside a Fable session)
// can't flip a 1M session to the 200k default and fire the urgent tier at
// ~20% of real usage. GC_CONTEXT_WINDOW_TOKENS overrides — gc-managed
// deployments that know the launch model should pin it for determinism.
func contextWindowTokens(models []string) int {
	if v := strings.TrimSpace(os.Getenv("GC_CONTEXT_WINDOW_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	best := 0
	for _, m := range models {
		if w := classifyWindow(m); w > best {
			best = w
		}
	}
	if best == 0 {
		return 200_000
	}
	return best
}

// classifyWindow maps one model string to its context window, delegating to
// modelwindow (the shared, single-place classifier — see its package doc for
// why the "[1m]" suffix alone isn't a reliable signal). A non-Claude or
// unrecognized model string falls back to the conservative 200k default.
func classifyWindow(model string) int {
	if window, ok := modelwindow.ForClaudeModel(model); ok {
		return window
	}
	return 200_000
}

// contextUsageMessage renders the guidance line for tokens used of window, or
// "" below the advisory threshold.
func contextUsageMessage(tokens, window int) string {
	if window <= 0 {
		return ""
	}
	advisory := thresholdPct("GC_CONTEXT_ADVISORY_PCT", 60)
	urgent := thresholdPct("GC_CONTEXT_URGENT_PCT", 80)
	pct := 100 * float64(tokens) / float64(window)
	k := func(n int) string { return fmt.Sprintf("%dk", (n+500)/1000) }
	switch {
	case pct < float64(advisory):
		return ""
	case pct <= float64(urgent):
		return fmt.Sprintf(
			"Context usage: %s/%s (~%.0f%%). Approaching the recycle zone. Steer toward a clean seam: finish in-flight work, don't open new long-horizon tasks, and keep durable notes/work-items current so a handoff is cheap. Plan to hand off and reset before this climbs into the urgent band — a fresh session from durable notes outperforms riding lossy compaction.\n",
			k(tokens), k(window), pct)
	case sessionIsEphemeral():
		// Pool/ephemeral workers do not own their own session lifecycle, so
		// `gc session reset` is an unrehearsed instruction for them: the
		// polecat contract's path is branch-ready -> refinery, or
		// `gc runtime request-restart` (sys-ykx0c). The push to COMMIT is
		// load-bearing rather than stylistic — uncommitted work in a polecat
		// worktree currently survives only because the witness salvage scan is
		// inert (sys-t4dfu), so telling a worker to stop without committing
		// would start losing work the moment that scan is fixed.
		return fmt.Sprintf(
			"Context usage: %s/%s (~%.0f%%) — HIGH. You are a pool worker: do NOT `gc session reset` — that is the named-identity path. Drive to your formula's handoff seam and COMMIT: for a polecat that is branch-ready — commit and push your branch, set your work-item metadata, hand off to the refinery, then exit (or `gc runtime request-restart`). Work left uncommitted in a worktree is not durable; do not rely on it surviving. Do this once you are at a seam; do NOT abandon work mid-step. (If an operator has told you to stay up, honor that and just hold at a clean seam instead.)\n",
			k(tokens), k(window), pct)
	default:
		return fmt.Sprintf(
			"Context usage: %s/%s (~%.0f%%) — HIGH. Recycle this session now: reach a clean seam, run your handoff (durable notes + work-item updates + memory), then `gc session reset` yourself to resume fresh from that durable state. Repeated compaction degrades awareness — a clean reset beats running to compaction. Do this once you are at a seam; do NOT abandon work mid-step. (If an operator has told you to stay up, honor that and just hold at a clean seam instead of resetting.)\n",
			k(tokens), k(window), pct)
	}
}

// sessionIsEphemeral reports whether this session is a pool worker rather than
// a configured named identity. gc stamps GC_SESSION_ORIGIN at spawn:
// "ephemeral" is the default for pool agents (template_resolve.go), and named
// identities are set to "named" (session_reconciler.go, build_desired_state.go).
// Anything else — including unset, which is how a manual invocation outside a
// managed session looks — is treated as named, so the conservative
// self-reset guidance stays the default.
func sessionIsEphemeral() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GC_SESSION_ORIGIN")), "ephemeral")
}

func thresholdPct(env string, def int) int {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			return n
		}
	}
	return def
}

// readHookStdin returns the provider hook input JSON from stdin when stdin is
// a pipe (the hook invocation shape). Interactive/manual invocations (stdin is
// a terminal) return nil so the command never blocks waiting for input.
func readHookStdin() []byte {
	st, err := os.Stdin.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return nil
	}
	return data
}
