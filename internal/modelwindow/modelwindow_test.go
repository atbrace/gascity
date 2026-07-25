package modelwindow

import "testing"

func TestForClaudeModel(t *testing.T) {
	tests := []struct {
		model  string
		want   int
		wantOK bool
	}{
		// Current (5-series) models: transcripts never carry "[1m]" for these
		// (see package doc), so family/version membership must be enough.
		{"claude-opus-5", 1_000_000, true},
		{"claude-sonnet-5", 1_000_000, true},

		// Older explicitly-1M generations, bare (no suffix in transcript).
		{"claude-opus-4-8", 1_000_000, true},
		{"claude-opus-4-7", 1_000_000, true},
		{"claude-opus-4-6", 1_000_000, true},
		{"claude-sonnet-4-6", 1_000_000, true},
		{"claude-fable-5", 1_000_000, true},
		{"mythos-1", 1_000_000, true},

		// Not-known-1M Claude variants stay at the conservative default.
		{"claude-opus-4-5-20251101", 200_000, true},
		{"claude-sonnet-4-5-20251101", 200_000, true},
		{"claude-haiku-4-5-20251001", 200_000, true},

		// Explicit "[1m]" suffix is authoritative even for unlisted variants.
		{"claude-haiku-4-5-20251001[1m]", 1_000_000, true},
		{"sonnet[1m]", 1_000_000, true},

		// Non-Claude / unrecognized: caller must apply its own fallback.
		{"gemini-2.5-pro", 0, false},
		{"gpt-5-20260101", 0, false},
		{"unknown-model-xyz", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			window, ok := ForClaudeModel(tt.model)
			if window != tt.want || ok != tt.wantOK {
				t.Errorf("ForClaudeModel(%q) = (%d, %v), want (%d, %v)", tt.model, window, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// Regression for sys-v3izb/gc-esqr: a 1M model ID with no "[1m]" suffix (the
// shape every real transcript actually has for the current model generation)
// must not resolve to the 200k default.
func TestForClaudeModelCurrentGenerationNeverFallsBackTo200k(t *testing.T) {
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5"} {
		window, ok := ForClaudeModel(model)
		if !ok {
			t.Fatalf("ForClaudeModel(%q) not recognized as Claude", model)
		}
		if window == DefaultWindow {
			t.Errorf("ForClaudeModel(%q) = %d, a 1M model must not resolve to the %d default", model, window, DefaultWindow)
		}
		if window != MillionTokenWindow {
			t.Errorf("ForClaudeModel(%q) = %d, want %d", model, window, MillionTokenWindow)
		}
	}
}
