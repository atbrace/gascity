package tmux

import "testing"

// TestProviderHasVerifiableBusyIndicator pins which provider families get
// submit confirmation. A codex worker whose submit Enter is lost drafts the
// nudge into the composer and then idles indefinitely while every liveness
// surface reads green — the same ga-bwm failure the Claude family already
// guards against. paneContainsBusyIndicator already recognizes codex's "esc to
// interrupt" footer, so the confirmation is available; only the eligibility
// gate was withholding it.
func TestProviderHasVerifiableBusyIndicator(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{"claude", true},
		{"codex", true},
		// codex-derived names resolve through the session-log family's
		// substring case. claude has no substring case, and GC_PROVIDER
		// already carries the resolved ancestor family for aliased claude
		// providers, so "claude-mini" correctly falls through.
		{"codex-max", true},
		{"claude-mini", false},
		// Providers whose busy indicator paneContainsBusyIndicator does not
		// recognize keep best-effort single delivery.
		{"grok", false},
		{"kimi", false},
		{"opencode", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := providerHasVerifiableBusyIndicator(tt.provider); got != tt.want {
			t.Errorf("providerHasVerifiableBusyIndicator(%q) = %v, want %v", tt.provider, got, tt.want)
		}
	}
}

// TestPaneContainsBusyIndicatorMatchesCodexWorkingFooter pins the codex busy
// footer as captured live from codex-cli 0.147.0 in a tmux pane. Submit
// confirmation for codex is only sound while this keeps matching.
func TestPaneContainsBusyIndicatorMatchesCodexWorkingFooter(t *testing.T) {
	codexWorking := []string{
		"› Reply with exactly the single word PONG and nothing else.",
		"• Working (3s • esc to interrupt)",
	}
	if !paneContainsBusyIndicator(codexWorking) {
		t.Fatal("codex working footer must read as busy, otherwise submit confirmation re-sends Enter into a live turn")
	}

	codexIdle := []string{
		"• PONG",
		"› Explain this codebase",
		"  gpt-5.5 low · /tmp/probe",
	}
	if paneContainsBusyIndicator(codexIdle) {
		t.Fatal("codex idle composer must not read as busy, otherwise a lost Enter is never re-sent")
	}
}
