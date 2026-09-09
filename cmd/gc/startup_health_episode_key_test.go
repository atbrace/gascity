package main

import "testing"

// A pool instance re-minted under a fresh session name per pending-create
// bead must accrue into ONE episode keyed by its qualified instance name
// (sys-by2243.12); named sessions without an instance fall back to the name.
func TestStartupHealthEpisodeKey_PoolInstanceStableAcrossRemint(t *testing.T) {
	tp := TemplateParams{TemplateName: "sysadmin/hudson", InstanceName: "sysadmin/hudson-3", PoolSlot: 3}
	if a, b := startupHealthEpisodeKey(tp, "hudson-gc-aaaaaa"), startupHealthEpisodeKey(tp, "hudson-gc-bbbbbb"); a != b || a != "sysadmin/hudson-3" {
		t.Fatalf("expected both mints to key to sysadmin/hudson-3, got %q and %q", a, b)
	}
	if got := startupHealthEpisodeKey(TemplateParams{}, "sky"); got != "sky" {
		t.Fatalf("expected fallback to session name, got %q", got)
	}
}
