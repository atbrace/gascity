package registry

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func fakeFactory(t *testing.T) (Factory, *int) {
	t.Helper()
	calls := 0
	return func(_ string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		calls++
		return runtime.NewFake(), nil
	}, &calls
}

func TestRegisterAndNewDispatchesByExactName(t *testing.T) {
	r := New()
	f, calls := fakeFactory(t)
	if err := r.Register("fake", f); err != nil {
		t.Fatalf("Register: %v", err)
	}
	p, err := r.New("fake", config.SessionConfig{}, "city", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p == nil {
		t.Fatal("New returned nil provider")
	}
	if *calls != 1 {
		t.Fatalf("factory calls = %d, want 1", *calls)
	}
}

func TestRegisterRejectsDuplicateName(t *testing.T) {
	r := New()
	f, _ := fakeFactory(t)
	if err := r.Register("fake", f); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register("fake", f)
	if err == nil {
		t.Fatal("duplicate Register succeeded, want error")
	}
	if !strings.Contains(err.Error(), "fake") {
		t.Fatalf("duplicate error %q does not name the colliding runtime", err)
	}
}

func TestRegisterRejectsEmptyNameAndNilFactory(t *testing.T) {
	r := New()
	f, _ := fakeFactory(t)
	if err := r.Register("", f); err == nil {
		t.Fatal("Register with empty name succeeded, want error")
	}
	if err := r.Register("  ", f); err == nil {
		t.Fatal("Register with blank name succeeded, want error")
	}
	if err := r.Register("fake", nil); err == nil {
		t.Fatal("Register with nil factory succeeded, want error")
	}
}

func TestRegisterPrefixDispatchesAndPassesFullName(t *testing.T) {
	r := New()
	var got string
	err := r.RegisterPrefix("exec:", func(name string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		got = name
		return runtime.NewFake(), nil
	})
	if err != nil {
		t.Fatalf("RegisterPrefix: %v", err)
	}
	if _, err := r.New("exec:/usr/local/bin/my-script", config.SessionConfig{}, "city", t.TempDir()); err != nil {
		t.Fatalf("New: %v", err)
	}
	if got != "exec:/usr/local/bin/my-script" {
		t.Fatalf("prefix factory received %q, want full selection name", got)
	}
}

func TestRegisterPrefixRejectsMalformedPrefix(t *testing.T) {
	r := New()
	f, _ := fakeFactory(t)
	if err := r.RegisterPrefix("exec", f); err == nil {
		t.Fatal("RegisterPrefix without trailing colon succeeded, want error")
	}
	if err := r.RegisterPrefix("", f); err == nil {
		t.Fatal("RegisterPrefix with empty prefix succeeded, want error")
	}
	if err := r.RegisterPrefix("exec:", nil); err == nil {
		t.Fatal("RegisterPrefix with nil factory succeeded, want error")
	}
}

func TestRegisterPrefixRejectsDuplicate(t *testing.T) {
	r := New()
	f, _ := fakeFactory(t)
	if err := r.RegisterPrefix("exec:", f); err != nil {
		t.Fatalf("first RegisterPrefix: %v", err)
	}
	if err := r.RegisterPrefix("exec:", f); err == nil {
		t.Fatal("duplicate RegisterPrefix succeeded, want error")
	}
}

func TestExactNameWinsOverPrefix(t *testing.T) {
	r := New()
	exact, exactCalls := fakeFactory(t)
	prefix, prefixCalls := fakeFactory(t)
	if err := r.Register("exec:special", exact); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.RegisterPrefix("exec:", prefix); err != nil {
		t.Fatalf("RegisterPrefix: %v", err)
	}
	if _, err := r.New("exec:special", config.SessionConfig{}, "city", t.TempDir()); err != nil {
		t.Fatalf("New: %v", err)
	}
	if *exactCalls != 1 || *prefixCalls != 0 {
		t.Fatalf("exact/prefix calls = %d/%d, want 1/0", *exactCalls, *prefixCalls)
	}
}

func TestUnknownNameUsesFallback(t *testing.T) {
	r := New()
	fb, calls := fakeFactory(t)
	r.SetFallback(fb)
	if _, err := r.New("definitely-unregistered", config.SessionConfig{}, "city", t.TempDir()); err != nil {
		t.Fatalf("New: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("fallback calls = %d, want 1", *calls)
	}
}

func TestUnknownNameWithoutFallbackErrors(t *testing.T) {
	r := New()
	_, err := r.New("definitely-unregistered", config.SessionConfig{}, "city", t.TempDir())
	if err == nil {
		t.Fatal("New for unregistered name without fallback succeeded, want error")
	}
	if !errors.Is(err, ErrUnknownRuntime) {
		t.Fatalf("err = %v, want ErrUnknownRuntime", err)
	}
	if !strings.Contains(err.Error(), "definitely-unregistered") {
		t.Fatalf("error %q does not name the unknown runtime", err)
	}
}

func TestFactoryErrorsPropagate(t *testing.T) {
	r := New()
	boom := errors.New("kubeconfig missing")
	err := r.Register("k8s", func(_ string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		return nil, boom
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err = r.New("k8s", config.SessionConfig{}, "city", t.TempDir())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped factory error", err)
	}
}

func TestHasAndNames(t *testing.T) {
	r := New()
	f, _ := fakeFactory(t)
	for _, name := range []string{"tmux", "fake", "acp"} {
		if err := r.Register(name, f); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}
	if !r.Has("fake") {
		t.Fatal("Has(fake) = false, want true")
	}
	if r.Has("nope") {
		t.Fatal("Has(nope) = true, want false")
	}
	got := r.Names()
	want := []string{"acp", "fake", "tmux"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want sorted %v", got, want)
		}
	}
}

func TestCloneIsIndependentOfOriginal(t *testing.T) {
	r := New()
	exact, _ := fakeFactory(t)
	prefix, _ := fakeFactory(t)
	fallback, fallbackCalls := fakeFactory(t)
	if err := r.Register("builtin", exact); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.RegisterPrefix("exec:", prefix); err != nil {
		t.Fatalf("RegisterPrefix: %v", err)
	}
	r.SetFallback(fallback)

	c := r.Clone()

	// The clone carries the original's registrations.
	if !c.Has("builtin") {
		t.Error("clone should carry exact registrations")
	}
	if _, err := c.New("exec:/x", config.SessionConfig{}, "city", t.TempDir()); err != nil {
		t.Errorf("clone prefix resolution: %v", err)
	}
	if _, err := c.New("unknown", config.SessionConfig{}, "city", t.TempDir()); err != nil {
		t.Errorf("clone fallback resolution: %v", err)
	}
	if *fallbackCalls != 1 {
		t.Errorf("fallback calls = %d, want 1 (clone shares the fallback factory)", *fallbackCalls)
	}

	// Registrations on the clone never leak back into the original —
	// pack runtimes from one city must not be visible to another.
	packRT, _ := fakeFactory(t)
	if err := c.Register("pack-runtime", packRT); err != nil {
		t.Fatalf("Register on clone: %v", err)
	}
	if r.Has("pack-runtime") {
		t.Error("registering on a clone must not mutate the original")
	}

	// Duplicate detection still applies on the clone.
	if err := c.Register("builtin", packRT); err == nil {
		t.Error("clone must reject names already registered in the original")
	}
}

func TestNewStrictNeverUsesFallback(t *testing.T) {
	r := New()
	fb, fallbackCalls := fakeFactory(t)
	r.SetFallback(fb)

	_, err := r.NewStrict("definitely-unregistered", config.SessionConfig{}, "city", t.TempDir())
	if err == nil {
		t.Fatal("NewStrict for an unregistered name succeeded, want error")
	}
	if !errors.Is(err, ErrUnknownRuntime) {
		t.Fatalf("err = %v, want ErrUnknownRuntime", err)
	}
	if !strings.Contains(err.Error(), "definitely-unregistered") {
		t.Fatalf("error %q does not name the unknown runtime", err)
	}
	if *fallbackCalls != 0 {
		t.Fatalf("fallback calls = %d, want 0: NewStrict must not reach the fallback", *fallbackCalls)
	}
}

func TestNewStrictResolvesExactAndPrefix(t *testing.T) {
	r := New()
	fb, fallbackCalls := fakeFactory(t)
	r.SetFallback(fb)
	exact, exactCalls := fakeFactory(t)
	if err := r.Register("nomad", exact); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var gotPrefixName string
	if err := r.RegisterPrefix("ssh:", func(name string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		gotPrefixName = name
		return runtime.NewFake(), nil
	}); err != nil {
		t.Fatalf("RegisterPrefix: %v", err)
	}

	if _, err := r.NewStrict("nomad", config.SessionConfig{}, "city", t.TempDir()); err != nil {
		t.Fatalf("NewStrict(nomad): %v", err)
	}
	if *exactCalls != 1 {
		t.Fatalf("exact factory calls = %d, want 1", *exactCalls)
	}
	if _, err := r.NewStrict("ssh:builder@host", config.SessionConfig{}, "city", t.TempDir()); err != nil {
		t.Fatalf("NewStrict(ssh:builder@host): %v", err)
	}
	if gotPrefixName != "ssh:builder@host" {
		t.Fatalf("prefix factory got name %q, want the full selection name", gotPrefixName)
	}
	if *fallbackCalls != 0 {
		t.Fatalf("fallback calls = %d, want 0", *fallbackCalls)
	}
}

func TestRebindReplacesRegisteredFactory(t *testing.T) {
	r := New()
	first, firstCalls := fakeFactory(t)
	if err := r.Register("hybrid", first); err != nil {
		t.Fatalf("Register: %v", err)
	}
	second, secondCalls := fakeFactory(t)
	if err := r.Rebind("hybrid", second); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	if _, err := r.New("hybrid", config.SessionConfig{}, "city", t.TempDir()); err != nil {
		t.Fatalf("New: %v", err)
	}
	if *firstCalls != 0 {
		t.Fatalf("replaced factory calls = %d, want 0", *firstCalls)
	}
	if *secondCalls != 1 {
		t.Fatalf("rebound factory calls = %d, want 1", *secondCalls)
	}
}

func TestRebindRejectsUnregisteredNameAndNilFactory(t *testing.T) {
	r := New()
	f, _ := fakeFactory(t)
	err := r.Rebind("never-registered", f)
	if err == nil {
		t.Fatal("Rebind of an unregistered name succeeded, want error")
	}
	if !strings.Contains(err.Error(), "never-registered") {
		t.Fatalf("error %q does not name the runtime", err)
	}
	if err := r.Register("hybrid", f); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Rebind("hybrid", nil); err == nil {
		t.Fatal("Rebind with nil factory succeeded, want error")
	}
}

func TestRebindOnCloneLeavesOriginalUntouched(t *testing.T) {
	orig := New()
	first, firstCalls := fakeFactory(t)
	if err := orig.Register("hybrid", first); err != nil {
		t.Fatalf("Register: %v", err)
	}
	clone := orig.Clone()
	second, secondCalls := fakeFactory(t)
	if err := clone.Rebind("hybrid", second); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	if _, err := orig.New("hybrid", config.SessionConfig{}, "city", t.TempDir()); err != nil {
		t.Fatalf("orig.New: %v", err)
	}
	if *firstCalls != 1 || *secondCalls != 0 {
		t.Fatalf("orig factory calls = %d/%d (orig/clone), want 1/0: rebinding a clone must not mutate the original", *firstCalls, *secondCalls)
	}
}
