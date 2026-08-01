package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// newCityScopePoolCity builds a city with a single CITY-SCOPE pool agent
// (Dir == "") that carries an import binding, has min=0, and NO custom
// scale_check — the shape of a pack-provided city-scope utility pool. Its
// qualified name is binding-qualified ("pack.dog") and its only store is the
// city store.
func newCityScopePoolCity(t *testing.T, maxActive int) (cfg *config.City, cityStore beads.Store, qualified string) {
	t.Helper()
	minSess := 0
	cfg = &config.City{
		Agents: []config.Agent{
			{
				Name:              "dog",
				BindingName:       "pack",
				MaxActiveSessions: &maxActive,
				MinActiveSessions: &minSess,
				// No ScaleCheck: default-probe pool.
				// No Dir: city scope.
				Provider: "mock",
			},
		},
		Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
	}
	return cfg, beads.NewMemStore(), "pack.dog"
}

func createRoutedWisp(t *testing.T, store beads.Store, id, routedTo string) {
	t.Helper()
	if _, err := store.Create(beads.Bead{
		ID:       id,
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": routedTo},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestBuildDesiredState_CityScopePool_RoutedDemandWakesPool guards the healthy
// path that gcy-zes was first suspected of breaking: a cold CITY-SCOPE pool
// with no custom scale_check does scale on routed city-store work. City scope
// is not the discriminator — a zero max_active_sessions is (see below).
func TestBuildDesiredState_CityScopePool_RoutedDemandWakesPool(t *testing.T) {
	cfg, cityStore, qualified := newCityScopePoolCity(t, 3)
	createRoutedWisp(t, cityStore, "wisp-1", qualified)

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, nil, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 1 {
		t.Errorf("city-scope pool demand = %d, want 1 (routed city-store bead must wake the cold pool)", got)
	}
	if len(result.State) != 1 {
		t.Errorf("desired sessions = %d, want 1", len(result.State))
	}
}

// TestBuildDesiredState_ZeroCapacityPool_WarnsOnRoutedDemand is the regression
// guard for gcy-zes. max_active_sessions=0 structurally disables a pool, which
// is a legitimate way to stop one — but routed work aimed at it then starves
// with no session, no demand line, and no error. That silence is the defect:
// it is indistinguishable from an empty queue, and it cost ten days of missed
// daily digests before anyone noticed. The reconciler must say so out loud.
func TestBuildDesiredState_ZeroCapacityPool_WarnsOnRoutedDemand(t *testing.T) {
	cfg, cityStore, qualified := newCityScopePoolCity(t, 0)
	createRoutedWisp(t, cityStore, "wisp-1", qualified)
	createRoutedWisp(t, cityStore, "wisp-2", qualified)

	var stderr bytes.Buffer
	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, nil, &sessionBeadSnapshot{}, nil, &stderr,
	)

	// The cap must still be honored — warning, not resurrection.
	if got := result.ScaleCheckCounts[qualified]; got != 0 {
		t.Errorf("zero-capacity pool demand = %d, want 0 (max_active_sessions=0 must stay structural)", got)
	}
	if len(result.State) != 0 {
		t.Errorf("desired sessions = %d, want 0 (a disabled pool must not be woken by the warning)", len(result.State))
	}

	logged := stderr.String()
	if !strings.Contains(logged, qualified) || !strings.Contains(logged, "max_active_sessions=0") {
		t.Errorf("stderr must name the starved template and the zero cap; got:\n%s", logged)
	}
	if !strings.Contains(logged, "2 unassigned routed bead(s)") {
		t.Errorf("stderr must report the starved bead count; got:\n%s", logged)
	}
}

// TestBuildDesiredState_ZeroCapacityPool_SilentWithoutDemand guards that the
// warning is demand-triggered. A deliberately disabled pool with an empty queue
// is a normal steady state and must not log on every patrol tick.
func TestBuildDesiredState_ZeroCapacityPool_SilentWithoutDemand(t *testing.T) {
	cfg, cityStore, _ := newCityScopePoolCity(t, 0)

	var stderr bytes.Buffer
	buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, nil, &sessionBeadSnapshot{}, nil, &stderr,
	)

	if logged := stderr.String(); strings.Contains(logged, "max_active_sessions=0") {
		t.Errorf("disabled pool with no routed work must not warn; got:\n%s", logged)
	}
}

// TestBuildDesiredState_ZeroCapacityPool_IgnoresOtherRoutes guards that work
// routed elsewhere never attributes starvation to a disabled pool.
func TestBuildDesiredState_ZeroCapacityPool_IgnoresOtherRoutes(t *testing.T) {
	cfg, cityStore, _ := newCityScopePoolCity(t, 0)
	createRoutedWisp(t, cityStore, "wisp-elsewhere", "pack.other")

	var stderr bytes.Buffer
	buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, nil, &sessionBeadSnapshot{}, nil, &stderr,
	)

	if logged := stderr.String(); strings.Contains(logged, "max_active_sessions=0") {
		t.Errorf("work routed to another template must not warn; got:\n%s", logged)
	}
}
