package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type submitTestHarness struct {
	store  *beads.MemStore
	ops    submitOps
	events []string
	branch string
	clean  bool
	ahead  int
	remote string
	head   string
	headErr   error
	headCalls int
}

func newSubmitTestHarness(t *testing.T, meta map[string]string) *submitTestHarness {
	t.Helper()
	h := &submitTestHarness{branch: "polecat/sys-1", clean: true, ahead: 2, head: "abc123", remote: "abc123"}
	h.store = beads.NewMemStoreFrom(1, []beads.Bead{{ID: "sys-1", Title: "work", Status: "in_progress", Assignee: "sysadmin/polecat-x", Metadata: meta}}, nil)
	rec := func(name string) { h.events = append(h.events, name) }
	h.ops = submitOps{
		ConvoyChildren: func(_ beads.Store, _ string) ([]beads.Bead, error) {
			b, _ := h.store.Get("sys-1")
			return []beads.Bead{b}, nil
		},
		RefineryConfigured: func(name string) bool { rec("refinery-check"); return name == "sysadmin/gastown.refinery" },
		CurrentBranch:      func(string) (string, error) { rec("current-branch"); return h.branch, nil },
		IsClean:            func(string) (bool, error) { rec("is-clean"); return h.clean, nil },
		FetchBase:          func(string, string) error { rec("fetch"); return nil },
		CommitsBeyond:      func(string, string) (int, error) { rec("commits-beyond"); return h.ahead, nil },
		Head:               func(string) (string, error) { h.headCalls++; return h.head, h.headErr },
		Push:               func(string, string) error { rec("push"); return nil },
		LsRemote:           func(string, string) (string, error) { rec("ls-remote"); return h.remote, nil },
		DeleteLocalBranch:  func(string, string) error { rec("delete-branch"); return nil },
		Wake:               func(string) error { rec("wake"); return nil },
		Nudge:              func(string, string) error { rec("nudge"); return nil },
	}
	return h
}

func (h *submitTestHarness) run(t *testing.T, opts submitOptions) (submitResult, error, string) {
	t.Helper()
	if opts.Refinery == "" {
		opts.Refinery = "sysadmin/gastown.refinery"
	}
	if opts.BaseBranch == "" {
		opts.BaseBranch = "main"
	}
	if opts.BeadID == "" && opts.ConvoyID == "" {
		opts.BeadID = "sys-1"
	}
	var stderr bytes.Buffer
	res, err := doSubmit(h.store, opts, h.ops, &stderr)
	return res, err, stderr.String()
}

func (h *submitTestHarness) bead(t *testing.T) beads.Bead {
	t.Helper()
	b, err := h.store.Get("sys-1")
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	return b
}

func (h *submitTestHarness) saw(name string) bool {
	for _, e := range h.events {
		if e == name {
			return true
		}
	}
	return false
}

func TestSubmitHappyPathWritesBeadOnlyAfterRemoteVerify(t *testing.T) {
	h := newSubmitTestHarness(t, nil)
	// Bead write is observed by the store; record its position relative to
	// push/ls-remote through the ops that bracket it.
	res, err, _ := h.run(t, submitOptions{})
	if err != nil {
		t.Fatalf("doSubmit: %v", err)
	}
	b := h.bead(t)
	if b.Assignee != "sysadmin/gastown.refinery" || b.Status != "open" {
		t.Fatalf("bead not handed off: assignee=%q status=%q", b.Assignee, b.Status)
	}
	if b.Metadata["branch"] != "polecat/sys-1" || b.Metadata["target"] != "main" || b.Metadata["gc.routed_to"] != "" {
		t.Fatalf("metadata = %v", b.Metadata)
	}
	if !res.HandedOff || res.Branch != "polecat/sys-1" || res.RemoteSHA != "abc123" {
		t.Fatalf("result = %+v", res)
	}
	want := []string{"refinery-check", "current-branch", "is-clean", "fetch", "commits-beyond", "push", "ls-remote", "delete-branch", "wake", "nudge"}
	if got := strings.Join(h.events, ","); got != strings.Join(want, ",") {
		t.Fatalf("op order = %s, want %s", got, strings.Join(want, ","))
	}
}

func TestSubmitRefusesDetachedHead(t *testing.T) {
	h := newSubmitTestHarness(t, nil)
	h.branch = ""
	_, err, _ := h.run(t, submitOptions{})
	if err == nil || !strings.Contains(err.Error(), "detached") {
		t.Fatalf("err = %v, want detached HEAD refusal", err)
	}
	assertSubmitUntouched(t, h)
}

func TestSubmitRefusesWrongBranch(t *testing.T) {
	h := newSubmitTestHarness(t, nil)
	h.branch = "main"
	_, err, _ := h.run(t, submitOptions{})
	if err == nil || !strings.Contains(err.Error(), "polecat/sys-1") {
		t.Fatalf("err = %v, want branch-shape refusal naming the expected branch", err)
	}
	assertSubmitUntouched(t, h)
}

func TestSubmitHonoursPresetMetadataBranch(t *testing.T) {
	h := newSubmitTestHarness(t, map[string]string{"branch": "feature/custom"})
	h.branch = "feature/custom"
	res, err, _ := h.run(t, submitOptions{})
	if err != nil {
		t.Fatalf("doSubmit: %v", err)
	}
	if res.Branch != "feature/custom" || h.bead(t).Metadata["branch"] != "feature/custom" {
		t.Fatalf("expected preset branch honoured, got %+v", res)
	}
}

func TestSubmitRefusesBaseBranchAsWorkBranch(t *testing.T) {
	h := newSubmitTestHarness(t, map[string]string{"branch": "main"})
	h.branch = "main"
	_, err, _ := h.run(t, submitOptions{})
	if err == nil || !strings.Contains(err.Error(), "base branch") {
		t.Fatalf("err = %v, want base-branch refusal", err)
	}
	assertSubmitUntouched(t, h)
}

func TestSubmitRefusesDirtyTree(t *testing.T) {
	h := newSubmitTestHarness(t, nil)
	h.clean = false
	_, err, _ := h.run(t, submitOptions{})
	if err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("err = %v, want dirty-tree refusal", err)
	}
	assertSubmitUntouched(t, h)
}

func TestSubmitRefusesNoCommitsBeyondBase(t *testing.T) {
	h := newSubmitTestHarness(t, nil)
	h.ahead = 0
	_, err, _ := h.run(t, submitOptions{})
	if err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("err = %v, want no-commits refusal", err)
	}
	assertSubmitUntouched(t, h)
}

func TestSubmitRefusesUnconfiguredRefineryBeforePushing(t *testing.T) {
	h := newSubmitTestHarness(t, nil)
	_, err, _ := h.run(t, submitOptions{Refinery: "sysadmin/refinery"})
	if err == nil || !strings.Contains(err.Error(), "sysadmin/refinery") {
		t.Fatalf("err = %v, want unconfigured-refinery refusal", err)
	}
	assertSubmitUntouched(t, h)
}

func TestSubmitPushFailureLeavesBeadUntouched(t *testing.T) {
	h := newSubmitTestHarness(t, nil)
	h.ops.Push = func(string, string) error { return errors.New("remote: permission denied") }
	_, err, _ := h.run(t, submitOptions{})
	if err == nil || !strings.Contains(err.Error(), "push") {
		t.Fatalf("err = %v, want push failure", err)
	}
	assertSubmitUntouched(t, h)
}

func TestSubmitRemoteMismatchLeavesBeadUntouched(t *testing.T) {
	h := newSubmitTestHarness(t, nil)
	h.remote = "def456"
	_, err, _ := h.run(t, submitOptions{})
	if err == nil || !strings.Contains(err.Error(), "def456") {
		t.Fatalf("err = %v, want remote-mismatch refusal", err)
	}
	if !h.saw("push") {
		t.Fatalf("push should have been attempted before verification")
	}
	assertSubmitUntouched(t, h)
}

func TestSubmitRemoteMissingLeavesBeadUntouched(t *testing.T) {
	h := newSubmitTestHarness(t, nil)
	h.remote = ""
	_, err, _ := h.run(t, submitOptions{})
	if err == nil || !strings.Contains(err.Error(), "no ref") {
		t.Fatalf("err = %v, want remote-missing refusal", err)
	}
	assertSubmitUntouched(t, h)
}

func TestSubmitAutoPushFalseHaltsAtBranchReadyWithoutPushing(t *testing.T) {
	h := newSubmitTestHarness(t, map[string]string{"auto_push": "false", "gc.routed_to": "sysadmin/polecat"})
	res, err, _ := h.run(t, submitOptions{})
	if err != nil {
		t.Fatalf("doSubmit: %v", err)
	}
	if res.HandedOff || !res.Halted {
		t.Fatalf("result = %+v, want halted", res)
	}
	if h.saw("push") || h.saw("ls-remote") {
		t.Fatalf("auto_push=false must not push: %v", h.events)
	}
	b := h.bead(t)
	if b.Assignee != "" || b.Status != "open" || b.Metadata["branch_ready"] != "true" || b.Metadata["halt_reason"] != "auto_push_false" || b.Metadata["gc.routed_to"] != "" || b.Metadata["branch"] != "polecat/sys-1" || b.Metadata["target"] != "main" {
		t.Fatalf("halt state wrong: assignee=%q status=%q meta=%v", b.Assignee, b.Status, b.Metadata)
	}
}

func TestSubmitResolvesSingleConvoyChild(t *testing.T) {
	h := newSubmitTestHarness(t, nil)
	res, err, _ := h.run(t, submitOptions{ConvoyID: "sys-convoy"})
	if err != nil {
		t.Fatalf("doSubmit: %v", err)
	}
	if res.BeadID != "sys-1" {
		t.Fatalf("bead = %q", res.BeadID)
	}
}

func TestSubmitRefusesAmbiguousConvoy(t *testing.T) {
	h := newSubmitTestHarness(t, nil)
	h.ops.ConvoyChildren = func(_ beads.Store, _ string) ([]beads.Bead, error) {
		return []beads.Bead{{ID: "sys-1"}, {ID: "sys-2"}}, nil
	}
	_, err, _ := h.run(t, submitOptions{ConvoyID: "sys-convoy"})
	if err == nil || !strings.Contains(err.Error(), "2 open members") {
		t.Fatalf("err = %v, want ambiguous-convoy refusal", err)
	}
	assertSubmitUntouched(t, h)
}

func TestSubmitWakeNudgeFailuresAreNonFatal(t *testing.T) {
	h := newSubmitTestHarness(t, nil)
	h.ops.Wake = func(string) error { return errors.New("no session") }
	h.ops.Nudge = func(string, string) error { return errors.New("no session") }
	res, err, stderr := h.run(t, submitOptions{})
	if err != nil || !res.HandedOff {
		t.Fatalf("handoff must succeed despite wake/nudge failure: err=%v res=%+v", err, res)
	}
	if !strings.Contains(stderr, "wake") {
		t.Fatalf("expected a wake warning on stderr, got %q", stderr)
	}
}

func assertSubmitUntouched(t *testing.T, h *submitTestHarness) {
	t.Helper()
	b := h.bead(t)
	if b.Assignee != "sysadmin/polecat-x" || b.Status != "in_progress" {
		t.Fatalf("bead was written despite refusal: assignee=%q status=%q", b.Assignee, b.Status)
	}
	if _, ok := b.Metadata["target"]; ok {
		t.Fatalf("metadata.target was written despite refusal: %v", b.Metadata)
	}
}

// ---- sys-pxryan.50: review gate in gc submit ----
//
// Authority for the current step is the persisted claim: gc.session_id /
// gc.session_name stamped on the non-closed step the session actually holds
// (exactly one, verified-submit ref, under the work bead's root). Authority
// for the approval is the terminal closed/pass ralph control: its
// gc.attempt_log settles the attempt, and that attempt's NATIVE review member
// (not the copied approve/SHA fields on apply/scope/scope-check descendants)
// must carry review.verdict=approve + review.reviewed_sha == candidate HEAD.

func newGuardHarness(t *testing.T, seeded []beads.Bead) *submitTestHarness {
	t.Helper()
	h := newSubmitTestHarness(t, nil)
	// Swap in a store seeded with the work + step + control + review beads.
	// The ConvoyChildren closure reads h.store dynamically, so it keeps working.
	h.store = beads.NewMemStoreFrom(1, seeded, nil)
	return h
}

// guardWork is the review_required work bead under workflow root sys-root.
func guardWork() beads.Bead {
	return beads.Bead{ID: "sys-1", Title: "work", Status: "in_progress", Assignee: "sysadmin/polecat-x",
		Metadata: map[string]string{"review_required": "true", "gc.dispatch_workflow": "sys-root"}}
}

// guardStep is a native formula step; sid "" means stamped to no session
// (preassigned but never claimed).
func guardStep(id, ref, status, sid string) beads.Bead {
	return beads.Bead{ID: id, Title: id, Status: status,
		Metadata: map[string]string{
			"gc.step_ref":     ref,
			"gc.root_bead_id": "sys-root",
			"gc.session_id":   sid,
		}}
}

// guardControl is the terminal closed/pass ralph control for review-loop
// under sys-root; settled names the attempt its gc.attempt_log settles on
// (last entry, the shape appendAttemptLogValue writes).
func guardControl(settled string) beads.Bead {
	return beads.Bead{ID: "sys-ctl", Title: "ralph control", Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id":  "sys-root",
			"gc.kind":          "ralph",
			"gc.step_id":       "review-loop",
			"gc.step_ref":      "homeops-work-reviewed.review-loop",
			"gc.outcome":       "pass",
			"gc.control_epoch": "1",
			"gc.attempt_log":   `[{"action":"close","attempt":"` + settled + `","outcome":"pass","reason":"review check: PASS"}]`,
		}}
}

// guardReviewMember is a native review-step bead for one iteration.
func guardReviewMember(attempt, verdict, sha string) beads.Bead {
	return beads.Bead{ID: "sys-rv-" + attempt, Title: "review", Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id":     "sys-root",
			"gc.step_id":          "review-loop.review",
			"gc.step_ref":         "review-loop.iteration." + attempt + ".review-loop.review",
			"gc.ralph_step_id":    "review-loop",
			"gc.attempt":          attempt,
			"gc.scope_ref":        "review-loop.iteration." + attempt,
			"gc.scope_role":       "member",
			"gc.outcome":          "pass",
			"review.verdict":      verdict,
			"review.reviewed_sha": sha,
		}}
}

// guardCopier is a descendant that COPIES approve/SHA fields but is not the
// native review member; kind + step_ref pick the copier shape (apply or
// scope-check, both observed in production roots).
func guardCopier(kind, stepID, ref, attempt, sha string) beads.Bead {
	return beads.Bead{ID: "sys-copy-" + stepID, Title: stepID, Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id":     "sys-root",
			"gc.kind":             kind,
			"gc.step_id":          stepID,
			"gc.step_ref":         ref,
			"gc.ralph_step_id":    "review-loop",
			"gc.attempt":          attempt,
			"gc.outcome":          "pass",
			"review.verdict":      "approve",
			"review.reviewed_sha": sha,
		}}
}

// runGuardSubmit invokes doSubmit as session gc-testsub, the session that
// claims the current step in each fixture.
func runGuardSubmit(t *testing.T, h *submitTestHarness) (submitResult, error, string) {
	t.Helper()
	t.Setenv("GC_SESSION_ID", "gc-testsub")
	t.Setenv("GC_SESSION_NAME", "homeops__gc-testsub")
	return h.run(t, submitOptions{BeadID: "sys-1"})
}

func assertGuardRefusal(t *testing.T, h *submitTestHarness, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got success")
	}
	if h.saw("push") || h.saw("ls-remote") {
		t.Fatalf("refusal must happen before push/verify; ops ran: %v", h.events)
	}
	if b := h.bead(t); b.Assignee == "sysadmin/gastown.refinery" {
		t.Fatalf("refusal must not write the refinery handoff: %v", b.Metadata)
	}
}

// The actual production shape (sys-kuth7c): claimed verified-submit step +
// terminal pass control settling attempt 1 + native review member approving
// exactly the candidate HEAD.
func TestReviewSubmitAllowsVerifiedSubmitWithApproval(t *testing.T) {
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		guardStep("sys-vs", "homeops-work-reviewed.verified-submit", "open", "gc-testsub"),
		guardControl("1"),
		guardReviewMember("1", "approve", "abc123"),
	})
	res, err, _ := runGuardSubmit(t, h)
	if err != nil {
		t.Fatalf("expected verified-submit + settled approval to pass, got: %v", err)
	}
	if !res.HandedOff || res.RemoteSHA != "abc123" {
		t.Fatalf("expected handoff at abc123, got %+v", res)
	}
	if !h.saw("push") {
		t.Fatalf("expected push to be recorded, got %v", h.events)
	}
	if b := h.bead(t); b.Assignee != "sysadmin/gastown.refinery" || b.Status != "open" {
		t.Fatalf("bead not handed off: assignee=%q status=%q", b.Assignee, b.Status)
	}
}

// The core sys-pxryan.38 regression: the apply step (implementer lane, same
// session) submits while a valid approval exists. The current claimed step is
// apply, not verified-submit -> refuse before push/write.
func TestReviewSubmitRefusesApplyStepNotVerifiedSubmit(t *testing.T) {
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		guardStep("sys-apply", "homeops-work-reviewed.review-loop.iteration.1.review-loop.apply", "open", "gc-testsub"),
		guardControl("1"),
		guardReviewMember("1", "approve", "abc123"),
	})
	_, err, _ := runGuardSubmit(t, h)
	assertGuardRefusal(t, h, err)
	if !strings.Contains(err.Error(), "verified-submit") {
		t.Fatalf("error should name the verified-submit step, got: %v", err)
	}
}

// Stale: the verified-submit step was already closed (a finished step cannot
// re-submit), even with a valid approval.
func TestReviewSubmitRefusesStaleClosedStep(t *testing.T) {
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		guardStep("sys-vs", "homeops-work-reviewed.verified-submit", "closed", "gc-testsub"),
		guardControl("1"),
		guardReviewMember("1", "approve", "abc123"),
	})
	_, err, _ := runGuardSubmit(t, h)
	assertGuardRefusal(t, h, err)
	if !strings.Contains(err.Error(), "no open step is claimed") {
		t.Fatalf("error should say no open claimed step, got: %v", err)
	}
}

// Mismatched session: the only verified-submit step is claimed by another
// session (gc-other). This session owns nothing -> refuse.
func TestReviewSubmitRefusesForeignStep(t *testing.T) {
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		guardStep("sys-vs", "homeops-work-reviewed.verified-submit", "open", "gc-other"),
		guardControl("1"),
		guardReviewMember("1", "approve", "abc123"),
	})
	_, err, _ := runGuardSubmit(t, h)
	assertGuardRefusal(t, h, err)
}

// Unstamped preassigned verified-submit (production regression): the Assignee
// field names THIS session, but the step was never actually claimed, so it has
// no hook-claim session stamp. Assignee alone is NOT authority -> refuse before
// push or any store mutation, even with a valid approval present.
func TestReviewSubmitRefusesUnstampedPreassignedStep(t *testing.T) {
	unstamped := guardStep("sys-vs", "homeops-work-reviewed.verified-submit", "open", "")
	unstamped.Metadata["gc.session_id"] = "" // never stamped by the claim path
	unstamped.Metadata["gc.session_name"] = ""
	unstamped.Assignee = "homeops__gc-testsub" // preassigned, but that is not a claim
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		unstamped,
		guardControl("1"),
		guardReviewMember("1", "approve", "abc123"),
	})
	_, err, _ := runGuardSubmit(t, h)
	assertGuardRefusal(t, h, err)
	if !strings.Contains(err.Error(), "no open step is claimed") {
		t.Fatalf("error should say no open step is claimed, got: %v", err)
	}
}

// Conflicting stamp: the step's gc.session_id names THIS session but its
// gc.session_name names a DIFFERENT session, and the assignee also matches this
// session. An inconsistent stamp is rejected even though the assignee matches ->
// refuse before push or store mutation.
func TestReviewSubmitRefusesConflictingStamp(t *testing.T) {
	conflict := guardStep("sys-vs", "homeops-work-reviewed.verified-submit", "open", "gc-testsub")
	conflict.Metadata["gc.session_name"] = "homeops__gc-other" // conflicts with id
	conflict.Assignee = "homeops__gc-testsub"                  // matches, but must not save it
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		conflict,
		guardControl("1"),
		guardReviewMember("1", "approve", "abc123"),
	})
	_, err, _ := runGuardSubmit(t, h)
	assertGuardRefusal(t, h, err)
	if !strings.Contains(err.Error(), "no open step is claimed") {
		t.Fatalf("error should say no open step is claimed (conflicting stamp), got: %v", err)
	}
}

// Mismatched root: the claimed verified-submit step belongs to a DIFFERENT
// workflow than the work bead being submitted -> refuse.
func TestReviewSubmitRefusesForeignRootStep(t *testing.T) {
	foreign := guardStep("sys-vs", "homeops-work-reviewed.verified-submit", "open", "gc-testsub")
	foreign.Metadata["gc.root_bead_id"] = "sys-other-root"
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		foreign,
		guardControl("1"),
		guardReviewMember("1", "approve", "abc123"),
	})
	_, err, _ := runGuardSubmit(t, h)
	assertGuardRefusal(t, h, err)
	if !strings.Contains(err.Error(), "not the verified-submit step") {
		t.Fatalf("error should name the current-step mismatch, got: %v", err)
	}
}

// Ambiguous: the session claims TWO open steps (apply + verified-submit) ->
// refuse, even though one of them is the right verified-submit.
func TestReviewSubmitRefusesMultipleClaimedSteps(t *testing.T) {
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		guardStep("sys-apply", "homeops-work-reviewed.review-loop.iteration.1.review-loop.apply", "open", "gc-testsub"),
		guardStep("sys-vs", "homeops-work-reviewed.verified-submit", "open", "gc-testsub"),
		guardControl("1"),
		guardReviewMember("1", "approve", "abc123"),
	})
	_, err, _ := runGuardSubmit(t, h)
	assertGuardRefusal(t, h, err)
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error should say the current step is ambiguous, got: %v", err)
	}
}

// An earlier OPEN step owned by ANOTHER session must not invalidate the one
// valid claimed verified-submit step -> allow.
func TestReviewSubmitIgnoresForeignEarlierStep(t *testing.T) {
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		guardStep("sys-earlier", "homeops-work-reviewed.isolated-implementation", "open", "gc-other"),
		guardStep("sys-vs", "homeops-work-reviewed.verified-submit", "open", "gc-testsub"),
		guardControl("1"),
		guardReviewMember("1", "approve", "abc123"),
	})
	res, err, _ := runGuardSubmit(t, h)
	if err != nil {
		t.Fatalf("a foreign open step must not block the claimed verified-submit; got: %v", err)
	}
	if !res.HandedOff {
		t.Fatalf("expected handoff, got %+v", res)
	}
}

// Review-required work with NO approval (no settled control, no review
// member) refuses before push and before any queue/handoff metadata write.
func TestReviewSubmitRefusesNoReviewApproval(t *testing.T) {
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		guardStep("sys-vs", "homeops-work-reviewed.verified-submit", "open", "gc-testsub"),
	})
	_, err, _ := runGuardSubmit(t, h)
	assertGuardRefusal(t, h, err)
	if !strings.Contains(err.Error(), "ralph control") {
		t.Fatalf("error should say the loop never settled, got: %v", err)
	}
}

// Copied wrong-step approval: apply + scope-check descendants carry approve +
// the right SHA, but the native review member only has iterate. Metadata
// copies are not independent review proof -> refuse.
func TestReviewSubmitRefusesCopiedWrongStepApproval(t *testing.T) {
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		guardStep("sys-vs", "homeops-work-reviewed.verified-submit", "open", "gc-testsub"),
		guardControl("1"),
		guardReviewMember("1", "iterate", "abc123"),
		guardCopier("", "review-loop.apply", "review-loop.iteration.1.review-loop.apply", "1", "abc123"),
		guardCopier("scope-check", "review-loop.review", "review-loop.iteration.1.review-loop.review-scope-check", "1", "abc123"),
	})
	_, err, _ := runGuardSubmit(t, h)
	assertGuardRefusal(t, h, err)
	if !strings.Contains(err.Error(), "review.verdict=approve") {
		t.Fatalf("error should say the native member has no approve verdict, got: %v", err)
	}
}

// Stale-attempt same SHA: the control settled attempt 2; the attempt-1 review
// member approved exactly the candidate SHA, but the settled (attempt-2)
// member only has iterate -> the stale approval must not bind -> refuse.
func TestReviewSubmitRefusesStaleAttemptSameSHA(t *testing.T) {
	ctl := guardControl("2")
	ctl.Metadata["gc.attempt_log"] = `[{"action":"retry","attempt":"1","outcome":"transient","reason":"iterate"}, {"action":"close","attempt":"2","outcome":"pass","reason":"review check: PASS"}]`
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		guardStep("sys-vs", "homeops-work-reviewed.verified-submit", "open", "gc-testsub"),
		ctl,
		guardReviewMember("1", "approve", "abc123"), // stale: same SHA, earlier attempt
		guardReviewMember("2", "iterate", "abc123"),
	})
	_, err, _ := runGuardSubmit(t, h)
	assertGuardRefusal(t, h, err)
	if !strings.Contains(err.Error(), "attempt 2") {
		t.Fatalf("refusal must bind to the settled attempt 2, got: %v", err)
	}
}

// No session identity at all (a raw manual `gc submit` outside any session)
// on review_required work -> refuse before push/write.
func TestReviewSubmitRefusesNoSessionIdentity(t *testing.T) {
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		guardStep("sys-vs", "homeops-work-reviewed.verified-submit", "open", "gc-testsub"),
		guardControl("1"),
		guardReviewMember("1", "approve", "abc123"),
	})
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_SESSION_NAME", "")
	_, err, _ := h.run(t, submitOptions{BeadID: "sys-1"})
	assertGuardRefusal(t, h, err)
	if !strings.Contains(err.Error(), "no session identity") {
		t.Fatalf("error should say no session identity, got: %v", err)
	}
}

// A review member approving a DIFFERENT SHA (work moved after review) ->
// refuse.
func TestReviewSubmitRefusesStaleReviewedSHA(t *testing.T) {
	h := newGuardHarness(t, []beads.Bead{
		guardWork(),
		guardStep("sys-vs", "homeops-work-reviewed.verified-submit", "open", "gc-testsub"),
		guardControl("1"),
		guardReviewMember("1", "approve", "deadbeef"),
	})
	_, err, _ := runGuardSubmit(t, h)
	assertGuardRefusal(t, h, err)
	if !strings.Contains(err.Error(), "candidate SHA") {
		t.Fatalf("error should name the SHA mismatch, got: %v", err)
	}
}

// Legacy (non review_required) work keeps the pre-gate flow: no session
// identity needed, no step/review beads, and no HEAD read on the auto_push
// halt path.
func TestSubmitLegacyAutoPushFalseDoesNotReadHead(t *testing.T) {
	h := newSubmitTestHarness(t, map[string]string{"auto_push": "false", "gc.routed_to": "sysadmin/polecat"})
	h.headErr = errors.New("head unavailable")
	res, err, _ := h.run(t, submitOptions{})
	if err != nil {
		t.Fatalf("legacy auto_push=false must halt cleanly, not fail: %v", err)
	}
	if !res.Halted || res.HaltReason != submitHaltReasonAutoPushFalse {
		t.Fatalf("result = %+v, want the branch-ready halt", res)
	}
	if h.headCalls != 0 {
		t.Fatalf("legacy auto_push=false must not read HEAD, got %d reads", h.headCalls)
	}
	if h.saw("push") || h.saw("ls-remote") {
		t.Fatalf("halt must not push or verify: %v", h.events)
	}
	b := h.bead(t)
	if b.Assignee != "" || b.Status != "open" || b.Metadata["branch_ready"] != "true" || b.Metadata["halt_reason"] != "auto_push_false" {
		t.Fatalf("halt state wrong: assignee=%q status=%q meta=%v", b.Assignee, b.Status, b.Metadata)
	}
}

// Legacy (non review_required) work still hands off with no guard at all.
func TestReviewSubmitLegacyUnchanged(t *testing.T) {
	h := newGuardHarness(t, []beads.Bead{
		{ID: "sys-1", Title: "work", Status: "in_progress", Assignee: "sysadmin/polecat-x",
			Metadata: map[string]string{"branch": "polecat/sys-1"}},
	})
	res, err, _ := h.run(t, submitOptions{BeadID: "sys-1"})
	if err != nil {
		t.Fatalf("legacy (non review_required) submit must still succeed, got: %v", err)
	}
	if !res.HandedOff {
		t.Fatalf("expected legacy submit to hand off, got %+v", res)
	}
}
