package rig

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestBoundImportsEqualIncludesAgentExclusions(t *testing.T) {
	base := []config.BoundImport{{
		Binding: "shared",
		Import:  config.Import{Source: "../shared", AgentsExclude: []string{"worker"}},
	}}
	if !boundImportsEqual(base, []config.BoundImport{{
		Binding: "shared",
		Import:  config.Import{Source: "../shared", AgentsExclude: []string{"worker"}},
	}}) {
		t.Fatal("identical bound imports should compare equal")
	}
	if boundImportsEqual(base, []config.BoundImport{{
		Binding: "shared",
		Import:  config.Import{Source: "../shared", AgentsExclude: []string{"keeper"}},
	}}) {
		t.Fatal("different agent exclusions should compare unequal")
	}
}

func TestMergeBoundImportsPreservesPrimaryAgentExclusionsOnSameSource(t *testing.T) {
	primary := []config.BoundImport{{
		Binding: "shared",
		Import:  config.Import{Source: "../shared", AgentsExclude: []string{"worker"}},
	}}
	secondary := []config.BoundImport{{
		Binding: "shared",
		Import:  config.Import{Source: "../shared"},
	}}
	merged, err := MergeBoundImports(primary, secondary)
	if err != nil {
		t.Fatalf("MergeBoundImports: %v", err)
	}
	if len(merged) != 1 || len(merged[0].Import.AgentsExclude) != 1 || merged[0].Import.AgentsExclude[0] != "worker" {
		t.Fatalf("merged imports = %#v, want primary agents_exclude preserved", merged)
	}
}
