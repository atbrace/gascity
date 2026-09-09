package dashboardbff

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recordingRoundTripper is a fake in-process transport standing in for the
// supervisor's LoopbackTransport: it records the request path and returns a
// canned response without touching the network, so a test can prove the
// samplers dispatch loopback reads through Deps.SelfReadTransport.
type recordingRoundTripper struct {
	gotPath string
	status  int
	body    string
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.gotPath = req.URL.Path
	code := rt.status
	if code == 0 {
		code = http.StatusOK
	}
	return &http.Response{
		StatusCode: code,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(rt.body)),
		Request:    req,
	}, nil
}

// TestSamplersUseSelfReadTransport is the regression test for the read-auth
// finding at the sampler layer: fetchStatus must dispatch its loopback status
// read through Deps.SelfReadTransport (the supervisor's in-process transport),
// not the network. The base URL is deliberately unroutable, so a networked read
// would fail; the canned status body proves the transport was used.
func TestSamplersUseSelfReadTransport(t *testing.T) {
	rt := &recordingRoundTripper{status: http.StatusOK, body: `{"store_health":{"size_bytes":42}}`}
	m := newSamplerManager(Deps{SupervisorBaseURL: "http://supervisor.invalid", SelfReadTransport: rt}, newExecRunner())

	raw, err := m.fetchStatus(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("fetchStatus via self-read transport: %v", err)
	}
	if rt.gotPath != "/v0/city/alpha/status" {
		t.Fatalf("transport saw path %q, want /v0/city/alpha/status", rt.gotPath)
	}
	if !strings.Contains(string(raw), "size_bytes") {
		t.Fatalf("fetchStatus body = %q, want the transport's canned status", raw)
	}
}

// statusServer returns an httptest server that serves a fixed supervisor status
// body at /v0/city/{name}/status, so refresh()'s fetchStatus succeeds.
func statusServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// TestRefreshReadersDoNotBlockOnProbe is the regression test for the HIGH
// finding: refresh() must not hold the per-city write lock across the blocking
// rig probe. beforeProbe blocks the probe pass mid-flight while a reader calls
// supervisorStatus(); if the write lock were held across probeRig, the reader's
// RLock would block until the probe is released and the deadline would elapse.
func TestRefreshReadersDoNotBlockOnProbe(t *testing.T) {
	srv := statusServer(t, `{"store_health":{"size_bytes":100},"rig_details":[{"name":"r1","path":"/dashboardbff-nonexistent-rig"}]}`)
	defer srv.Close()

	m := newSamplerManager(Deps{SupervisorBaseURL: srv.URL}, newExecRunner())
	cs := &citySampler{name: "alpha", mgr: m}

	probing := make(chan struct{}) // closed once the probe pass is in-flight
	release := make(chan struct{}) // test closes this to let the probe finish
	cs.beforeProbe = func() {
		close(probing)
		<-release
	}

	done := make(chan struct{})
	go func() {
		cs.refresh(context.Background())
		close(done)
	}()

	select {
	case <-probing:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh never reached the rig probe")
	}

	// The probe is mid-flight. A reader must still return promptly — proving no
	// write lock is held across probeRig.
	got := make(chan supervisorStatusReport, 1)
	go func() { got <- cs.supervisorStatus() }()
	select {
	case <-got:
		// reader returned while the probe is blocked: contract upheld.
	case <-time.After(time.Second):
		t.Fatal("supervisorStatus() blocked while a probe was in flight: write lock held across probeRig")
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not finish after probe released")
	}
}

// TestRefreshPublishesUnderLock confirms the happy path still publishes status,
// the dolt ring, and the rig report after one refresh.
func TestRefreshPublishesUnderLock(t *testing.T) {
	srv := statusServer(t, `{"store_health":{"size_bytes":4096},"rig_details":[{"name":"r1","path":"/dashboardbff-nonexistent-rig"}]}`)
	defer srv.Close()

	m := newSamplerManager(Deps{SupervisorBaseURL: srv.URL}, newExecRunner())
	cs := &citySampler{name: "alpha", mgr: m}
	cs.refresh(context.Background())

	if rep := cs.supervisorStatus(); !rep.Available {
		t.Errorf("supervisorStatus available = false, want true after a good fetch")
	}
	trend := cs.doltTrend()
	if !trend.Available || len(trend.Samples) != 1 || trend.Samples[0].Bytes != 4096 {
		t.Errorf("doltTrend = %+v, want one 4096-byte sample available", trend)
	}
	rig := cs.rigStoreHealth()
	if !rig.Available || len(rig.Rigs) != 1 {
		t.Errorf("rigStoreHealth = %+v, want one rig available", rig)
	}
	// The probed rig dir does not exist, so it rolls up down/unreachable.
	if rig.Rigs[0].Reachable {
		t.Errorf("rig reachable = true, want false for a missing .beads dir")
	}
}

// TestRefreshDegradesNotBlankOnFetchError verifies a failed status fetch retains
// the last-good snapshot (status flips to unavailable, dolt/rig data survives).
func TestRefreshDegradesNotBlankOnFetchError(t *testing.T) {
	srv := statusServer(t, `{"store_health":{"size_bytes":2048},"rig_details":[{"name":"r1","path":"/dashboardbff-nonexistent-rig"}]}`)
	m := newSamplerManager(Deps{SupervisorBaseURL: srv.URL}, newExecRunner())
	cs := &citySampler{name: "alpha", mgr: m}

	cs.refresh(context.Background()) // seed last-good
	srv.Close()                      // next fetch fails
	cs.refresh(context.Background())

	if rep := cs.supervisorStatus(); rep.Available {
		t.Errorf("supervisorStatus available = true, want false after fetch failure")
	} else if rep.Reason != "status_read_failed" {
		t.Errorf("reason = %q, want status_read_failed", rep.Reason)
	}
	// Last-good dolt + rig data must survive the failed fetch (degrade, not blank).
	if trend := cs.doltTrend(); len(trend.Samples) != 1 {
		t.Errorf("doltTrend samples = %d, want 1 retained after fetch failure", len(trend.Samples))
	}
	if rig := cs.rigStoreHealth(); len(rig.Rigs) != 1 {
		t.Errorf("rigStoreHealth rigs = %d, want 1 retained after fetch failure", len(rig.Rigs))
	}
}

// TestRefreshCadenceGates confirms the dolt ring only appends on its 10-min
// cadence: two back-to-back refreshes append once (the second is inside the
// window), while the rig probe (5-min cadence) likewise runs once.
func TestRefreshCadenceGates(t *testing.T) {
	srv := statusServer(t, `{"store_health":{"size_bytes":100},"rig_details":[]}`)
	defer srv.Close()

	m := newSamplerManager(Deps{SupervisorBaseURL: srv.URL}, newExecRunner())
	cs := &citySampler{name: "alpha", mgr: m}

	cs.refresh(context.Background())
	first := cs.doltTrend()
	cs.refresh(context.Background()) // within doltAppendInterval: no new sample
	second := cs.doltTrend()

	if len(first.Samples) != 1 || len(second.Samples) != 1 {
		t.Errorf("dolt ring grew inside the append window: first=%d second=%d", len(first.Samples), len(second.Samples))
	}
}

// TestEnsureDoesNotStoreCityPath documents that ensure no longer tracks the
// city path (the dead cs.path reassignment was removed); the sampler keys off
// cs.name and rig paths come from the status body.
func TestEnsureDoesNotStoreCityPath(t *testing.T) {
	m := newSamplerManager(Deps{}, newExecRunner())
	cs := m.ensure("alpha")
	if cs.name != "alpha" {
		t.Errorf("ensure name = %q, want alpha", cs.name)
	}
	// Calling ensure again returns the same sampler instance.
	if again := m.ensure("alpha"); again != cs {
		t.Error("ensure should return the cached sampler for a known city")
	}
}

// TestDoctorHoldAfterTimeout is the regression test for the orphaned-query
// leak: the bd doctor client timeout kills bd but not the query it issued on
// the dolt server, so a rig whose probe timed out must be held out of the
// next probe passes instead of re-issuing the same query every 5 minutes.
func TestDoctorHoldAfterTimeout(t *testing.T) {
	rig := t.TempDir()
	if err := os.Mkdir(filepath.Join(rig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	beadsPath := filepath.Join(rig, ".beads")
	srv := statusServer(t, `{"rig_details":[{"name":"r1","path":"`+rig+`"}]}`)
	defer srv.Close()

	m := newSamplerManager(Deps{SupervisorBaseURL: srv.URL}, newExecRunner())
	calls := 0
	m.doctor = func(context.Context, string) (*execResult, error) {
		calls++
		return nil, &execError{msg: "exec timed out", kind: execErrTimeout}
	}
	cs := &citySampler{name: "alpha", mgr: m}

	now := time.Now()
	cs.refresh(context.Background())
	if calls != 1 {
		t.Fatalf("first pass ran doctor %d times, want 1", calls)
	}
	if !cs.doctorHeld(beadsPath, now.Add(doctorHoldInitial-time.Second)) {
		t.Fatal("rig not held after a doctor timeout")
	}
	if cs.doctorHeld(beadsPath, now.Add(doctorHoldInitial+time.Second)) {
		t.Fatal("hold did not expire after doctorHoldInitial")
	}

	// A held pass must not fork bd, and must say so in the note.
	cs.lastRig = time.Time{} // re-open the 5-min cadence gate
	cs.refresh(context.Background())
	if calls != 1 {
		t.Fatalf("held pass ran doctor (calls=%d), want 0 extra", calls)
	}
	rep := cs.rigStoreHealth()
	if len(rep.Rigs) != 1 || !strings.Contains(rep.Rigs[0].Note, "held") {
		t.Fatalf("held pass report = %+v, want a 'held' note", rep.Rigs)
	}

	// Consecutive timeouts double the hold up to the cap; a success clears it.
	for i := 0; i < 10; i++ {
		cs.noteDoctorResult(beadsPath, true, now)
	}
	if got := cs.doctorHolds[beadsPath].backoff; got != doctorHoldMax {
		t.Fatalf("backoff after repeated timeouts = %v, want cap %v", got, doctorHoldMax)
	}
	cs.noteDoctorResult(beadsPath, false, now)
	if cs.doctorHeld(beadsPath, now) {
		t.Fatal("hold survived a successful probe")
	}
}

// TestProbeRigTimeoutIsNotAHoldForOtherErrors: only the timeout kind arms a
// hold; a spawn failure (bd missing) has no server-side query to protect.
func TestDoctorHoldIgnoresSpawnErrors(t *testing.T) {
	rig := t.TempDir()
	if err := os.Mkdir(filepath.Join(rig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := newSamplerManager(Deps{}, newExecRunner())
	m.doctor = func(context.Context, string) (*execResult, error) {
		return nil, &execError{msg: "spawn failed", kind: execErrSpawn}
	}
	if _, timedOut := m.probeRig(context.Background(), "r1", rig, true); timedOut {
		t.Fatal("spawn failure reported as timeout")
	}
}
