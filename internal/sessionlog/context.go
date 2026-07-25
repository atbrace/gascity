// Package sessionlog reads Claude Code JSONL session files for
// lightweight metadata extraction (model, context usage).
package sessionlog

import (
	"strings"

	"github.com/gastownhall/gascity/internal/modelwindow"
)

// nonClaudeFamilyWindows maps non-Claude model family keywords to their
// context window sizes. Claude models are classified by modelwindow, the
// shared single source of truth (also used by cmd/gc's context-usage hook),
// so a newly-shipped Claude generation only needs to be added there.
var nonClaudeFamilyWindows = map[string]int{
	"gemini": 1_000_000,
	"gpt-5":  258_000,
	"codex":  258_000,
	"gpt-4":  128_000,
	"gpt-4o": 128_000,
}

// ModelContextWindow returns the context window size for a model ID.
// It parses the model ID to extract the family name and looks it up.
// Returns 0 if the model family is unknown.
func ModelContextWindow(model string) int {
	if window, ok := modelwindow.ForClaudeModel(model); ok {
		return window
	}
	lower := strings.ToLower(model)
	// Try longer matches first to avoid "gpt-4" matching before "gpt-4o".
	for _, family := range []string{"gpt-4o", "gpt-5", "gpt-4", "gemini", "codex"} {
		if strings.Contains(lower, family) {
			return nonClaudeFamilyWindows[family]
		}
	}
	return 0
}
