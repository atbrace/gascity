package main

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

var errProbeUnavailable = errors.New("fingerprint probe unavailable")

// fingerprintProbeStore wraps a real store with a controllable
// ActiveFingerprint, and counts probes so a test can distinguish "reused the
// snapshot after one cheap probe" from "rebuilt".
type fingerprintProbeStore struct {
	beads.Store
	mu     sync.Mutex
	fp     string
	err    error
	probes int
}

func (f *fingerprintProbeStore) ActiveFingerprint() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes++
	return f.fp, f.err
}

func (f *fingerprintProbeStore) set(fp string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fp = fp
}

func (f *fingerprintProbeStore) probeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probes
}

// plainProbeStore implements no fingerprinting at all, standing in for a store
// type that cannot answer the cheap question.
type plainProbeStore struct{ beads.Store }

// installControlReadyStore points controlReadyCacheFor at a fixed store and
// counts opens (an open per call == a rebuild per call). It resets the shared
// registry before and after so tests do not leak snapshots into each other.
func installControlReadyStore(t *testing.T, store beads.Store) func() int {
	t.Helper()
	resetControlReadyCacheRegistry()
	var mu sync.Mutex
	opens := 0
	prev := openControlReadyStore
	openControlReadyStore = func(string, string, *config.City) (beads.Store, error) {
		mu.Lock()
		opens++
		mu.Unlock()
		return store, nil
	}
	t.Cleanup(func() {
		openControlReadyStore = prev
		resetControlReadyCacheRegistry()
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return opens
	}
}

func resetControlReadyCacheRegistry() {
	controlReadyCacheRegistry.mu.Lock()
	controlReadyCacheRegistry.byDir = make(map[string]*controlReadyCacheEntry)
	controlReadyCacheRegistry.mu.Unlock()
}

// expireControlReadyEntry ages an entry past the TTL without sleeping, so the
// next call takes the revalidation path.
func expireControlReadyEntry(t *testing.T, dir string, age time.Duration) *controlReadyCacheEntry {
	t.Helper()
	controlReadyCacheRegistry.mu.Lock()
	defer controlReadyCacheRegistry.mu.Unlock()
	entry, ok := controlReadyCacheRegistry.byDir[dir]
	if !ok {
		t.Fatalf("no control-ready cache entry for %s", dir)
	}
	entry.primedAt = entry.primedAt.Add(-age)
	return entry
}

func TestControlReadyCacheReusesSnapshotWhenFingerprintUnchanged(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	probe := &fingerprintProbeStore{Store: store, fp: "n1|mx1|nb1|"}
	opens := installControlReadyStore(t, probe)

	first := controlReadyCacheFor(cityDir, cityDir, nil)
	if first == nil {
		t.Fatalf("first controlReadyCacheFor returned nil, want a primed cache")
	}
	if got := opens(); got != 1 {
		t.Fatalf("opens after first call = %d, want 1", got)
	}

	expireControlReadyEntry(t, cityDir, 2*controlReadyCacheTTL)

	second := controlReadyCacheFor(cityDir, cityDir, nil)
	if second != first {
		t.Fatalf("expired-but-unchanged cache was rebuilt; want the same snapshot reused")
	}
	if got := opens(); got != 1 {
		t.Fatalf("opens after revalidation = %d, want 1 (a matching fingerprint must not rebuild)", got)
	}
	// One probe to capture the fingerprint at build time, one to revalidate.
	if got := probe.probeCount(); got != 2 {
		t.Fatalf("fingerprint probes = %d, want 2 (one at build, one at revalidation)", got)
	}
}

// TestControlReadyCacheRebuildsWhenFingerprintChanges is the guard on the
// staleness trap documented on gcy-gla: reusing a snapshot across a real
// change would leave closed beads visible as ready, because
// CachingStore.PrimeActive absorbs and never evicts.
func TestControlReadyCacheRebuildsWhenFingerprintChanges(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	probe := &fingerprintProbeStore{Store: store, fp: "n1|mx1|nb1|"}
	opens := installControlReadyStore(t, probe)

	first := controlReadyCacheFor(cityDir, cityDir, nil)
	if first == nil {
		t.Fatalf("first controlReadyCacheFor returned nil, want a primed cache")
	}

	expireControlReadyEntry(t, cityDir, 2*controlReadyCacheTTL)
	probe.set("n0|mx2|nb0|") // e.g. the one ready bead closed

	second := controlReadyCacheFor(cityDir, cityDir, nil)
	if second == first {
		t.Fatalf("changed fingerprint reused the stale snapshot; want a rebuild")
	}
	if got := opens(); got != 2 {
		t.Fatalf("opens after change = %d, want 2 (a changed fingerprint must rebuild)", got)
	}
}

func TestControlReadyCacheRebuildsPastMaxAgeEvenWhenFingerprintMatches(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	probe := &fingerprintProbeStore{Store: store, fp: "n1|mx1|nb1|"}
	opens := installControlReadyStore(t, probe)

	if controlReadyCacheFor(cityDir, cityDir, nil) == nil {
		t.Fatalf("first controlReadyCacheFor returned nil, want a primed cache")
	}

	entry := expireControlReadyEntry(t, cityDir, 2*controlReadyCacheTTL)
	controlReadyCacheRegistry.mu.Lock()
	entry.builtAt = entry.builtAt.Add(-2 * controlReadyCacheMaxAge)
	controlReadyCacheRegistry.mu.Unlock()

	if controlReadyCacheFor(cityDir, cityDir, nil) == nil {
		t.Fatalf("second controlReadyCacheFor returned nil, want a rebuilt cache")
	}
	if got := opens(); got != 2 {
		t.Fatalf("opens past max age = %d, want 2 (max age must force a rebuild)", got)
	}
}

func TestControlReadyCacheRebuildsWhenProbeFailsOrIsUnavailable(t *testing.T) {
	t.Run("probe error", func(t *testing.T) {
		cityDir, store := setUpControlReadyFileStoreCity(t)
		probe := &fingerprintProbeStore{Store: store, fp: "n1|mx1|nb1|"}
		opens := installControlReadyStore(t, probe)

		if controlReadyCacheFor(cityDir, cityDir, nil) == nil {
			t.Fatalf("first controlReadyCacheFor returned nil, want a primed cache")
		}
		expireControlReadyEntry(t, cityDir, 2*controlReadyCacheTTL)

		probe.mu.Lock()
		probe.err = errProbeUnavailable
		probe.mu.Unlock()

		if controlReadyCacheFor(cityDir, cityDir, nil) == nil {
			t.Fatalf("second controlReadyCacheFor returned nil, want a rebuilt cache")
		}
		if got := opens(); got != 2 {
			t.Fatalf("opens after probe error = %d, want 2 (an unusable probe must rebuild)", got)
		}
	})

	t.Run("store cannot fingerprint", func(t *testing.T) {
		cityDir, store := setUpControlReadyFileStoreCity(t)
		opens := installControlReadyStore(t, &plainProbeStore{Store: store})

		if controlReadyCacheFor(cityDir, cityDir, nil) == nil {
			t.Fatalf("first controlReadyCacheFor returned nil, want a primed cache")
		}
		expireControlReadyEntry(t, cityDir, 2*controlReadyCacheTTL)

		if controlReadyCacheFor(cityDir, cityDir, nil) == nil {
			t.Fatalf("second controlReadyCacheFor returned nil, want a rebuilt cache")
		}
		if got := opens(); got != 2 {
			t.Fatalf("opens without a fingerprinter = %d, want 2 (no cheap path means rebuild)", got)
		}
	})
}

// TestControlReadyCacheWithinTTLDoesNotProbe pins the hot path: inside the TTL
// the snapshot is served with no probe at all, exactly as before this change.
func TestControlReadyCacheWithinTTLDoesNotProbe(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	probe := &fingerprintProbeStore{Store: store, fp: "n1|mx1|nb1|"}
	opens := installControlReadyStore(t, probe)

	first := controlReadyCacheFor(cityDir, cityDir, nil)
	if first == nil {
		t.Fatalf("first controlReadyCacheFor returned nil, want a primed cache")
	}
	probesAfterBuild := probe.probeCount()

	if second := controlReadyCacheFor(cityDir, cityDir, nil); second != first {
		t.Fatalf("call inside TTL did not return the same snapshot")
	}
	if got := opens(); got != 1 {
		t.Fatalf("opens inside TTL = %d, want 1", got)
	}
	if got := probe.probeCount(); got != probesAfterBuild {
		t.Fatalf("probes inside TTL = %d, want %d (the hot path must not probe)", got, probesAfterBuild)
	}
}
