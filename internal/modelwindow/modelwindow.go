// Package modelwindow is the single source of truth for mapping a Claude
// model ID string to its context-window size in tokens.
//
// Both the gc CLI's context-usage hook (cmd/gc/context_inject.go) and the
// session-log dashboard tooling (internal/sessionlog) need this
// classification. It used to be duplicated in both places with two
// independently-drifting lists of "which model is 1M"; this package is the
// one place that list lives now, so a newly-shipped model family only needs
// to be added once.
package modelwindow

import "strings"

// MillionTokenWindow is the context window for 1M-token Claude variants.
const MillionTokenWindow = 1_000_000

// DefaultWindow is the conservative window for a recognized Claude model
// whose generation/variant isn't known to be 1M.
const DefaultWindow = 200_000

// claudeFamilies are the model-ID substrings that identify a string as a
// Claude model at all (as opposed to some other provider, or garbage).
var claudeFamilies = []string{"opus", "sonnet", "haiku", "fable", "mythos"}

// unconditionalMillion lists Claude model-ID substrings whose
// generation/variant ships with a 1M context window regardless of whether
// the launch-time "[1m]" suffix survives into the logged model string.
//
// The suffix does NOT reliably survive: Claude Code session transcripts log
// the bare API model name (e.g. "claude-opus-5"), never a "[1m]" launch
// annotation — confirmed empirically against live transcripts (sys-v3izb):
// a mayor session running claude-opus-5 with a 1M window logged
// message.model as exactly "claude-opus-5" in every usage entry, with no
// suffix anywhere in the transcript. A classifier that requires spotting
// "[1m]" in a transcript-sourced model string can never fire for the
// families that actually depend on it, so those families are listed here
// and treated as 1M unconditionally. Pin GC_CONTEXT_WINDOW_TOKENS when a
// new model's window isn't yet recognized here.
var unconditionalMillion = []string{
	"fable", "mythos",
	"opus-4-6", "opus-4-7", "opus-4-8",
	"sonnet-4-6",
	"opus-5", "sonnet-5",
}

// ForClaudeModel returns the context-window size for a Claude model ID. ok
// is false when model isn't a recognized Claude family at all (some other
// provider, or an unrecognized string), in which case window is 0 and the
// caller should apply its own default/fallback for non-Claude models.
//
// An explicit "[1m]" suffix is authoritative if present; otherwise
// generation/variant membership in unconditionalMillion decides, falling
// back to the conservative 200k default for a recognized-but-unlisted
// Claude variant.
func ForClaudeModel(model string) (window int, ok bool) {
	lower := strings.ToLower(model)
	isClaude := false
	for _, family := range claudeFamilies {
		if strings.Contains(lower, family) {
			isClaude = true
			break
		}
	}
	if !isClaude {
		return 0, false
	}
	if strings.Contains(lower, "[1m]") {
		return MillionTokenWindow, true
	}
	for _, s := range unconditionalMillion {
		if strings.Contains(lower, s) {
			return MillionTokenWindow, true
		}
	}
	return DefaultWindow, true
}
