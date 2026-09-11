package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func triggerAffinityOpts() hookClaimOptions {
	return hookClaimOptions{
		Assignee:            "pool/session-1",
		TriggerBeadID:       "trigger-1",
		TriggerBeadStoreRef: "rig:target",
		JSON:                true,
		DrainAck:            true,
	}
}

func triggerAffinityOps(claim func(string) (beads.Bead, bool, error)) hookClaimOps {
	return hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			return claim(beadID)
		},
		DrainAck: func(_ io.Writer) error { return nil },
		LookupTrigger: func(context.Context, string, []string, string) (beads.Bead, error) {
			return beads.Bead{}, beads.ErrNotFound
		},
		ReadyContinuation: func(context.Context, string, []string, string) ([]beads.Bead, error) {
			return nil, nil
		},
	}
}

func triggerAffinityJSON(t *testing.T, bead beads.Bead) string {
	t.Helper()
	data, err := json.Marshal([]beads.Bead{bead})
	if err != nil {
		t.Fatalf("marshal trigger fixture: %v", err)
	}
	return string(data)
}

func triggerAffinityBead(id, status, assignee string) beads.Bead {
	return beads.Bead{ID: id, Status: status, Assignee: assignee}
}

func runTriggerAffinity(t *testing.T, opts hookClaimOptions, stores []hookStore, run hookStoreRunner, claim func(string) (beads.Bead, bool, error)) (hookClaimJSONResult, int, []string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	var calls []string
	wrapped := func(command, dir string, env []string) (string, error) {
		calls = append(calls, dir)
		return run(command, dir, env)
	}
	code := claimHookWorkWithRunner("bd ready --json", "fallback", nil, stores, opts, triggerAffinityOps(claim), wrapped, nil, &stdout, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("result is not JSON: %v; stdout=%q", err, stdout.String())
	}
	return result, code, calls
}

func TestTriggerHookClaimDoesNotFallBackToUnrelatedSource(t *testing.T) {
	opts := triggerAffinityOpts()
	stores := []hookStore{
		{dir: "city", storeRef: "city:test"},
		{dir: "target", storeRef: "rig:target"},
		{dir: "other", storeRef: "rig:other"},
	}
	result, code, calls := runTriggerAffinity(t, opts, stores, func(_ string, dir string, _ []string) (string, error) {
		if dir != "target" {
			t.Fatalf("unexpected query against %q", dir)
		}
		return triggerAffinityJSON(t, triggerAffinityBead("unrelated", "open", "")), nil
	}, func(string) (beads.Bead, bool, error) {
		t.Fatal("unrelated trigger path attempted a claim")
		return beads.Bead{}, false, nil
	})
	if code != 0 || result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("result=%+v code=%d, want acknowledged no_work drain", result, code)
	}
	if len(calls) != 1 || calls[0] != "target" {
		t.Fatalf("queried stores=%v, want exactly [target]", calls)
	}
}

func TestTriggerHookClaimMissingTerminalHeldAndForeignAreNoWork(t *testing.T) {
	cases := []struct {
		name string
		bead beads.Bead
	}{
		{name: "missing", bead: beads.Bead{}},
		{name: "terminal", bead: triggerAffinityBead("trigger-1", "closed", "")},
		{name: "foreign assignment", bead: triggerAffinityBead("trigger-1", "open", "other/session")},
		{name: "hold", bead: beads.Bead{ID: "trigger-1", Status: "open", Metadata: map[string]string{"gc.hold": "true"}}},
		{name: "future defer", bead: beads.Bead{ID: "trigger-1", Status: "open", DeferUntil: func() *time.Time { future := time.Now().Add(time.Hour); return &future }()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := triggerAffinityOpts()
			stores := []hookStore{{dir: "target", storeRef: "rig:target"}, {dir: "other", storeRef: "rig:other"}}
			result, code, calls := runTriggerAffinity(t, opts, stores, func(_ string, dir string, _ []string) (string, error) {
				if dir != "target" {
					t.Fatalf("unexpected query against %q", dir)
				}
				if tc.name == "missing" {
					return `[]`, nil
				}
				return triggerAffinityJSON(t, tc.bead), nil
			}, func(string) (beads.Bead, bool, error) {
				t.Fatal("non-claimable trigger attempted a claim")
				return beads.Bead{}, false, nil
			})
			if code != 0 || result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
				t.Fatalf("result=%+v code=%d, want acknowledged no_work drain", result, code)
			}
			if len(calls) != 1 || calls[0] != "target" {
				t.Fatalf("queried stores=%v, want exactly [target]", calls)
			}
		})
	}
}

func TestTriggerHookClaimStoreFailureIsStructuredRetry(t *testing.T) {
	opts := triggerAffinityOpts()
	stores := []hookStore{{dir: "target", storeRef: "rig:target"}, {dir: "other", storeRef: "rig:other"}}
	result, code, calls := runTriggerAffinity(t, opts, stores, func(_ string, _ string, _ []string) (string, error) {
		return "", errors.New("store unavailable")
	}, func(string) (beads.Bead, bool, error) {
		t.Fatal("store failure attempted a claim")
		return beads.Bead{}, false, nil
	})
	if code != 0 || result.Action != "drain" || result.Reason != hookClaimReasonRetry {
		t.Fatalf("result=%+v code=%d, want acknowledged retry drain", result, code)
	}
	if len(calls) != 1 || calls[0] != "target" {
		t.Fatalf("queried stores=%v, want exactly [target]", calls)
	}
}

func TestTriggerHookClaimClaimsExactTrigger(t *testing.T) {
	opts := triggerAffinityOpts()
	stores := []hookStore{{dir: "target", storeRef: "rig:target"}, {dir: "other", storeRef: "rig:other"}}
	var claimedID string
	result, code, calls := runTriggerAffinity(t, opts, stores, func(_ string, dir string, _ []string) (string, error) {
		if dir != "target" {
			t.Fatalf("unexpected query against %q", dir)
		}
		return triggerAffinityJSON(t, triggerAffinityBead("trigger-1", "open", "")), nil
	}, func(id string) (beads.Bead, bool, error) {
		claimedID = id
		return triggerAffinityBead(id, "in_progress", opts.Assignee), true, nil
	})
	if code != 0 || result.Action != "work" || result.Reason != "claimed" || result.BeadID != "trigger-1" {
		t.Fatalf("result=%+v code=%d, want claimed exact trigger", result, code)
	}
	if claimedID != opts.TriggerBeadID {
		t.Fatalf("claimed id=%q, want %q", claimedID, opts.TriggerBeadID)
	}
	if len(calls) != 1 || calls[0] != "target" {
		t.Fatalf("queried stores=%v, want exactly [target]", calls)
	}
}

func TestTriggerHookClaimReturnsExistingExactAssignment(t *testing.T) {
	opts := triggerAffinityOpts()
	stores := []hookStore{{dir: "target", storeRef: "rig:target"}}
	claimCalled := false
	result, code, _ := runTriggerAffinity(t, opts, stores, func(_ string, _ string, _ []string) (string, error) {
		return triggerAffinityJSON(t, triggerAffinityBead("trigger-1", "in_progress", opts.Assignee)), nil
	}, func(string) (beads.Bead, bool, error) {
		claimCalled = true
		return beads.Bead{}, false, nil
	})
	if code != 0 || result.Action != "work" || result.Reason != "existing_assignment" || result.BeadID != opts.TriggerBeadID {
		t.Fatalf("result=%+v code=%d, want existing exact assignment", result, code)
	}
	if claimCalled {
		t.Fatal("existing exact assignment must not be claimed again")
	}
}

func TestTriggerHookClaimKeepsExistingAssignmentWhenInputIsTerminal(t *testing.T) {
	opts := triggerAffinityOpts()
	stores := []hookStore{{dir: "target", storeRef: "rig:target"}}
	inputDoneCalled := false
	skipCalled := false
	var stdout, stderr bytes.Buffer
	ops := triggerAffinityOps(func(string) (beads.Bead, bool, error) {
		t.Fatal("existing exact assignment must not be claimed again")
		return beads.Bead{}, false, nil
	})
	ops.InputDone = func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
		inputDoneCalled = true
		return "root-1", true, nil
	}
	ops.SkipDoneWorkflow = func(context.Context, string, []string, string) error {
		skipCalled = true
		return nil
	}
	code := claimHookWorkWithRunner("bd ready --json", "fallback", nil, stores, opts, ops,
		func(_ string, _ string, _ []string) (string, error) {
			bead := triggerAffinityBead("trigger-1", "in_progress", opts.Assignee)
			bead.Metadata = map[string]string{"gc.root_bead_id": "root-1", "gc.input_convoy_id": "convoy-1"}
			return triggerAffinityJSON(t, bead), nil
		}, nil, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("result is not JSON: %v; stdout=%q", err, stdout.String())
	}
	if code != 0 || result.Action != "work" || result.Reason != "existing_assignment" || result.BeadID != opts.TriggerBeadID {
		t.Fatalf("result=%+v code=%d, want existing exact assignment", result, code)
	}
	if inputDoneCalled || skipCalled {
		t.Fatalf("trigger-bound existing assignment invoked input retirement: inputDone=%t skip=%t", inputDoneCalled, skipCalled)
	}
}

func TestTriggerHookClaimPreassignsOnlyFromInitialTrigger(t *testing.T) {
	opts := triggerAffinityOpts()
	opts.RouteTargets = []string{"pool"}
	stores := []hookStore{{dir: "target", storeRef: "rig:target"}}
	var assigned string
	ops := triggerAffinityOps(func(id string) (beads.Bead, bool, error) {
		return triggerAffinityBead(id, "in_progress", opts.Assignee), true, nil
	})
	ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
		return []beads.Bead{{ID: "continuation-b", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "pool"}}}, nil
	}
	ops.AssignContinuation = func(_ context.Context, _ string, _ []string, id, assignee string) error {
		assigned = id + ":" + assignee
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("bd ready --json", "fallback", nil, stores, opts, ops,
		func(_ string, _ string, _ []string) (string, error) {
			bead := triggerAffinityBead(opts.TriggerBeadID, "open", "")
			bead.Metadata = map[string]string{
				beadmeta.RootBeadIDMetadataKey: "root-1",
				beadmeta.ContinuationGroupMetadataKey: "group-1",
			}
			return triggerAffinityJSON(t, bead), nil
		}, nil, &stdout, &stderr)
	if code != 0 || assigned != "continuation-b:"+opts.Assignee {
		t.Fatalf("code=%d assigned=%q, want initial trigger to preassign B", code, assigned)
	}
}

func TestTriggerHookClaimUsesOnlyDurablyAssignedContinuation(t *testing.T) {
	cases := []struct {
		name     string
		siblings []beads.Bead
		wantWork bool
	}{
		{name: "same session", siblings: []beads.Bead{{ID: "continuation-b", Status: "open", Assignee: "pool/session-1"}}, wantWork: true},
		{name: "same session in progress", siblings: []beads.Bead{{ID: "continuation-b", Status: "in_progress", Assignee: "pool/session-1"}}, wantWork: true},
		{name: "foreign", siblings: []beads.Bead{{ID: "continuation-b", Status: "open", Assignee: "other/session"}}},
		{name: "unassigned", siblings: []beads.Bead{{ID: "continuation-b", Status: "open"}}},
		{name: "unrelated", siblings: []beads.Bead{{ID: "continuation-c", Status: "open", Assignee: "pool/session-1"}}},
		{name: "blocked before ready", siblings: []beads.Bead{{ID: "continuation-b", Status: "open", Assignee: "pool/session-1"}, {ID: "continuation-d", Status: "open", Assignee: "pool/session-1"}}, wantWork: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := triggerAffinityOpts()
			stores := []hookStore{{dir: "target", storeRef: "rig:target"}, {dir: "other", storeRef: "rig:other"}}
			ops := triggerAffinityOps(func(string) (beads.Bead, bool, error) {
				t.Fatal("continuation path must not claim")
				return beads.Bead{}, false, nil
			})
			ops.LookupTrigger = func(context.Context, string, []string, string) (beads.Bead, error) {
				return beads.Bead{ID: opts.TriggerBeadID, Status: "closed", Metadata: map[string]string{
					beadmeta.RootBeadIDMetadataKey: "root-1", beadmeta.ContinuationGroupMetadataKey: "group-1",
				}}, nil
			}
			ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
				for i := range tc.siblings {
					if tc.siblings[i].ID == "continuation-b" || tc.siblings[i].ID == "continuation-d" {
						tc.siblings[i].Metadata = map[string]string{
							beadmeta.RootBeadIDMetadataKey: "root-1", beadmeta.ContinuationGroupMetadataKey: "group-1",
						}
					}
				}
				return tc.siblings, nil
			}
			ops.ReadyContinuation = func(context.Context, string, []string, string) ([]beads.Bead, error) {
				if tc.name == "same session in progress" { return nil, nil }
				if tc.name == "blocked before ready" {
					return []beads.Bead{{ID: "continuation-d", Status: "open", Assignee: opts.Assignee}}, nil
				}
				if tc.wantWork {
					return []beads.Bead{{ID: "continuation-b", Status: "open", Assignee: opts.Assignee}}, nil
				}
				return nil, nil
			}
			var stdout, stderr bytes.Buffer
			calls := 0
			code := claimHookWorkWithRunner("bd ready --json", "fallback", nil, stores, opts, ops,
				func(_ string, dir string, _ []string) (string, error) {
					calls++
					if dir != "target" { t.Fatalf("queried unrelated store %q", dir) }
					return `[]`, nil
				}, nil, &stdout, &stderr)
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil { t.Fatalf("result is not JSON: %v", err) }
			if tc.wantWork {
				wantID := "continuation-b"
				if tc.name == "blocked before ready" { wantID = "continuation-d" }
				if code != 0 || result.Action != "work" || result.BeadID != wantID { t.Fatalf("result=%+v code=%d", result, code) }
			} else if code != 0 || result.Action != "drain" || result.Reason != hookClaimReasonNoWork { t.Fatalf("result=%+v code=%d, want no_work", result, code) }
			if calls != 1 { t.Fatalf("query calls=%d, want 1 exact store query", calls) }
		})
	}
}
