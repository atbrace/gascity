package registry

import (
	"reflect"
	"strings"
	"testing"
)

// TestResolverMethodSetIsResolutionOnly pins Resolver's method set. The
// providerledger guard sanctions exactly one registry escape from
// buildRuntimeRegistry — an explicit conversion to Resolver — on the strength
// of this interface being unable to reach catalog mutation. That guarantee
// holds only while the method set stays resolution-only, so growing it is a
// deliberate act that must confront this test, not a drive-by.
func TestResolverMethodSetIsResolutionOnly(t *testing.T) {
	typ := reflect.TypeOf((*Resolver)(nil)).Elem()
	if typ.NumMethod() != 1 {
		var names []string
		for i := 0; i < typ.NumMethod(); i++ {
			names = append(names, typ.Method(i).Name)
		}
		t.Fatalf("Resolver must stay resolution-only: got %d methods (%v), want exactly [NewStrict]", typ.NumMethod(), names)
	}
	method := typ.Method(0)
	if method.Name != "NewStrict" {
		t.Fatalf("Resolver method = %q, want NewStrict", method.Name)
	}
	forbidden := []string{"Register", "RegisterPrefix", "SetFallback", "Rebind", "Clone"}
	for _, name := range forbidden {
		if _, ok := typ.MethodByName(name); ok {
			t.Fatalf("Resolver exposes mutation-capable method %s", name)
		}
	}
	// No return type may hand the registry back — that would let a holder of
	// the read-only surface recover the mutable one.
	for i := 0; i < method.Type.NumOut(); i++ {
		if out := method.Type.Out(i).String(); strings.Contains(out, "registry.Registry") {
			t.Fatalf("Resolver.%s return %d is %s: must not expose the registry", method.Name, i, out)
		}
	}
}
