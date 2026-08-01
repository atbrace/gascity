package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/registry"
)

// hybridArmPkg reports the package of a hybrid provider's local or remote
// arm ("local" or "remote"). hybrid.Provider keeps its arms unexported, so
// this reads the field's dynamic type by reflection — Type() is legal on an
// unexported field even though Interface() is not — and it inspects the
// composition without invoking either backend.
func hybridArmPkg(t *testing.T, p runtime.Provider, field string) string {
	t.Helper()
	v := reflect.ValueOf(p)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		t.Fatalf("provider %T is not a non-nil pointer", p)
	}
	arm := v.Elem().FieldByName(field)
	if !arm.IsValid() {
		t.Fatalf("provider %T has no %q field; hybrid arm layout changed", p, field)
	}
	if arm.Kind() != reflect.Interface || arm.IsNil() {
		t.Fatalf("hybrid %s arm is not a populated provider interface", field)
	}
	got := arm.Elem().Type().String() // e.g. "*exec.seamBackedProvider"
	pkg, _, ok := strings.Cut(strings.TrimPrefix(got, "*"), ".")
	if !ok {
		t.Fatalf("hybrid %s arm type %q has no package qualifier", field, got)
	}
	return pkg
}

// packRuntimeCity returns a city config declaring one pack runtime `name`
// backed by a script that appends each op it receives to the returned marker
// path, so tests can prove which sessions actually reached that runtime.
func packRuntimeCity(t *testing.T, name string) (*config.City, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ops.log")
	script := writeRPPScript(t, fmt.Sprintf(`echo "$1 $2" >> %q
case "$1" in is-running) echo false ;; *) exit 2 ;; esac
`, marker))
	return &config.City{Runtimes: map[string]config.DiscoveredRuntime{
		name: {Name: name, Command: script, PackName: "clusterpack", PackDir: filepath.Dir(script)},
	}}, marker
}

func TestHybridRemoteName_DefaultsToK8s(t *testing.T) {
	cases := []struct {
		configured string
		want       string
	}{
		{"", "k8s"},            // back-compat: the historical hardcoded arm
		{"   ", "k8s"},         // whitespace-only is not a selection
		{"k8s", "k8s"},         // explicit, same as the default
		{"nomad", "nomad"},     // pack-declared runtime
		{"  nomad  ", "nomad"}, // trimmed
		{"ssh:box:22", "ssh:box:22"},
	}
	for _, c := range cases {
		got := hybridRemoteName(config.SessionConfig{HybridRemote: c.configured})
		if got != c.want {
			t.Errorf("hybridRemoteName(%q) = %q, want %q", c.configured, got, c.want)
		}
	}
}

// TestNewHybridProvider_RemoteArmResolvesPackRuntime is the point of the
// change: hybrid's remote arm resolves through the per-city registry, so a
// pack-declared runtime (the Nomad case) can back it while the local arm
// stays tmux.
func TestNewHybridProvider_RemoteArmResolvesPackRuntime(t *testing.T) {
	cfg, marker := packRuntimeCity(t, "nomad")
	sc := config.SessionConfig{HybridRemote: "nomad", RemoteMatch: "polecat"}

	reg, err := runtimeRegistryForCity(cfg)
	if err != nil {
		t.Fatalf("runtimeRegistryForCity: %v", err)
	}
	p, err := reg.New("hybrid", sc, "city", t.TempDir())
	if err != nil {
		t.Fatalf("New(hybrid): %v", err)
	}

	if got := hybridArmPkg(t, p, "remote"); got != "exec" {
		t.Errorf("remote arm package = %q, want %q (the pack runtime's exec proxy)", got, "exec")
	}
	if got := hybridArmPkg(t, p, "local"); got != "tmux" {
		t.Errorf("local arm package = %q, want %q", got, "tmux")
	}

	// The wiring is real, not merely well-typed: a remote-matched session
	// reaches the declared command.
	p.IsRunning("gastown__polecat-abc")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("declared command was never invoked for a remote-matched session: %v", err)
	}
	if !strings.Contains(string(data), "is-running gastown__polecat-abc") {
		t.Errorf("ops log = %q, want the remote-matched session routed to the pack runtime", data)
	}
}

// TestNewHybridProvider_UnknownRemoteFailsLoudly pins the hazard this change
// exists to avoid: an unresolvable remote arm must be a construction error,
// never the registry's tmux fallback, which would silently run
// remote-matched sessions on the control-plane host.
func TestNewHybridProvider_UnknownRemoteFailsLoudly(t *testing.T) {
	sc := config.SessionConfig{HybridRemote: "nomad", RemoteMatch: "polecat"}
	// No [runtimes.nomad] declared anywhere in this city.
	reg, err := runtimeRegistryForCity(nil)
	if err != nil {
		t.Fatalf("runtimeRegistryForCity: %v", err)
	}
	p, err := reg.New("hybrid", sc, "city", t.TempDir())
	if err == nil {
		t.Fatalf("New(hybrid) with unregistered remote arm succeeded (arm = %q), want error",
			hybridArmPkg(t, p, "remote"))
	}
	if p != nil {
		t.Errorf("provider = %T, want nil alongside the error", p)
	}
	if !errors.Is(err, registry.ErrUnknownRuntime) {
		t.Errorf("err = %v, want ErrUnknownRuntime", err)
	}
	if !strings.Contains(err.Error(), "nomad") {
		t.Errorf("error %q does not name the unresolvable remote arm", err)
	}
}

// TestNewHybridProvider_RejectsSelfAsRemote guards the one selection name
// that would recurse: hybrid resolving hybrid.
func TestNewHybridProvider_RejectsSelfAsRemote(t *testing.T) {
	sc := config.SessionConfig{HybridRemote: "hybrid", RemoteMatch: "polecat"}
	reg, err := runtimeRegistryForCity(nil)
	if err != nil {
		t.Fatalf("runtimeRegistryForCity: %v", err)
	}
	p, err := reg.New("hybrid", sc, "city", t.TempDir())
	if err == nil {
		t.Fatal("New(hybrid) with hybrid_remote = \"hybrid\" succeeded, want error")
	}
	if p != nil {
		t.Errorf("provider = %T, want nil alongside the error", p)
	}
	if !strings.Contains(err.Error(), "hybrid") {
		t.Errorf("error %q does not explain the nesting", err)
	}
}

// TestRuntimeRegistryForCity_RebindsHybridToTheClone proves the rebind, not
// just its effect: the clone's hybrid factory must resolve against the clone
// (pack runtimes included), while the process-global registry keeps its own
// binding and stays free of the pack runtime.
func TestRuntimeRegistryForCity_RebindsHybridToTheClone(t *testing.T) {
	cfg, _ := packRuntimeCity(t, "nomad")
	reg, err := runtimeRegistryForCity(cfg)
	if err != nil {
		t.Fatalf("runtimeRegistryForCity: %v", err)
	}
	if reg == runtimeRegistry {
		t.Fatal("a city with pack runtimes must get a clone, not the global registry")
	}
	if runtimeRegistry.Has("nomad") {
		t.Fatal("pack runtime leaked into the process-global registry")
	}

	sc := config.SessionConfig{HybridRemote: "nomad", RemoteMatch: "polecat"}
	if _, err := reg.New("hybrid", sc, "city", t.TempDir()); err != nil {
		t.Fatalf("clone New(hybrid) with a pack-runtime remote arm: %v", err)
	}
	if _, err := runtimeRegistry.New("hybrid", sc, "city", t.TempDir()); err == nil {
		t.Error("global registry resolved a pack-declared remote arm; its hybrid binding was mutated")
	}
}
