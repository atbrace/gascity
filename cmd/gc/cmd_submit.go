package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	gitpkg "github.com/gastownhall/gascity/internal/git"
)

// gc submit is the polecat's refinery handoff as ONE atomic verb. The shell it
// replaces (mol-polecat-work submit-and-exit) carried three fail-closed gates —
// branch shape, push exit code, ls-remote verify — that an agent could skip on
// its way to the bead write that LOOKS like completion (metadata.branch set,
// assignee=refinery). Here the bead write is unreachable unless the pushed ref
// was read back from origin at the expected SHA, so "handed off" means "the
// branch is on origin" by construction rather than by the agent's cooperation.

type submitOptions struct {
	Dir        string
	BeadID     string
	ConvoyID   string
	BaseBranch string
	Refinery   string
}

type submitOps struct {
	ConvoyChildren     func(store beads.Store, convoyID string) ([]beads.Bead, error)
	RefineryConfigured func(name string) bool
	CurrentBranch      func(dir string) (string, error) // "" when HEAD is detached
	IsClean            func(dir string) (bool, error)
	FetchBase          func(dir, base string) error
	CommitsBeyond      func(dir, base string) (int, error)
	Head               func(dir string) (string, error)
	Push               func(dir, branch string) error
	LsRemote           func(dir, branch string) (string, error) // "" when the ref is absent
	DeleteLocalBranch  func(dir, branch string) error
	Wake               func(target string) error
	Nudge              func(target, message string) error
}

type submitResult struct {
	BeadID     string `json:"bead_id"`
	Branch     string `json:"branch"`
	Target     string `json:"target"`
	Refinery   string `json:"refinery,omitempty"`
	LocalSHA   string `json:"local_sha,omitempty"`
	RemoteSHA  string `json:"remote_sha,omitempty"`
	HandedOff  bool   `json:"handed_off"`
	Halted     bool   `json:"halted"`
	HaltReason string `json:"halt_reason,omitempty"`
}

const submitHaltReasonAutoPushFalse = "auto_push_false"

func newSubmitCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts submitOptions
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "submit",
		Short: "Hand a polecat's work to the refinery atomically (push, verify on origin, then reassign)",
		Long: `Perform the polecat -> refinery handoff as one fail-closed operation.

In order: resolve the work bead (--bead, or the single open member of
--convoy); check the refinery target names a configured agent; assert HEAD is
on the expected branch (metadata.branch, else polecat/<bead>) and not detached;
assert the tree is clean; fetch the base and assert the branch has commits
beyond it; push; read the ref back from origin with ls-remote and require it
at the local HEAD. Only then does it write metadata.branch/target, clear
gc.routed_to and reassign the bead to the refinery. Any failure before that
point leaves the bead exactly as it was.

If the bead carries metadata.auto_push=false the verb halts at branch-ready
without pushing or reassigning, matching the previous formula contract.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if opts.Dir == "" {
				dir, err := os.Getwd()
				if err != nil {
					fmt.Fprintf(stderr, "gc submit: %v\n", err) //nolint:errcheck
					return errExit
				}
				opts.Dir = dir
			}
			res, err := runSubmit(opts, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc submit: %v\n", err) //nolint:errcheck
				return errExit
			}
			if jsonOutput {
				return json.NewEncoder(stdout).Encode(res)
			}
			if res.Halted {
				fmt.Fprintf(stdout, "halted at branch-ready: %s (%s, no push, no refinery handoff)\n", res.Branch, res.HaltReason) //nolint:errcheck
				return nil
			}
			fmt.Fprintf(stdout, "handed off %s: %s@%s verified on origin, assigned to %s\n", res.BeadID, res.Branch, res.RemoteSHA, res.Refinery) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.BeadID, "bead", "", "work bead id (or use --convoy)")
	cmd.Flags().StringVar(&opts.ConvoyID, "convoy", "", "convoy whose single open member is the work bead")
	cmd.Flags().StringVar(&opts.BaseBranch, "base", "main", "base branch the refinery merges into (metadata.target)")
	cmd.Flags().StringVar(&opts.Refinery, "refinery", "", "qualified refinery agent name to assign the bead to (required)")
	cmd.Flags().StringVar(&opts.Dir, "dir", "", "worktree directory (default: cwd)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "JSON output")
	_ = cmd.MarkFlagRequired("refinery")
	return cmd
}

func runSubmit(opts submitOptions, stderr io.Writer) (submitResult, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return submitResult{}, err
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		return submitResult{}, err
	}
	store := hookClaimBdStore(opts.Dir, os.Environ(), "gc-submit")
	ops := submitOps{
		ConvoyChildren: func(s beads.Store, convoyID string) ([]beads.Bead, error) {
			return listConvoyChildren(s, convoyID, false)
		},
		RefineryConfigured: func(name string) bool {
			_, ok := findAgentByQualified(cfg, name)
			return ok
		},
		CurrentBranch: func(dir string) (string, error) {
			out, err := submitGit(dir, "rev-parse", "--abbrev-ref", "HEAD")
			if err != nil {
				return "", err
			}
			if out == "HEAD" {
				return "", nil
			}
			return out, nil
		},
		IsClean: func(dir string) (bool, error) {
			out, err := submitGit(dir, "status", "--porcelain")
			return out == "", err
		},
		FetchBase: func(dir, base string) error {
			_, err := submitGit(dir, "fetch", "origin", base)
			return err
		},
		CommitsBeyond: func(dir, base string) (int, error) {
			out, err := submitGit(dir, "rev-list", "--count", "origin/"+base+"..HEAD")
			if err != nil {
				return 0, err
			}
			var n int
			if _, err := fmt.Sscanf(out, "%d", &n); err != nil {
				return 0, fmt.Errorf("parsing rev-list count %q: %w", out, err)
			}
			return n, nil
		},
		Head: func(dir string) (string, error) { return submitGit(dir, "rev-parse", "HEAD") },
		Push: func(dir, branch string) error {
			_, err := submitGit(dir, "push", "origin", "HEAD:refs/heads/"+branch)
			return err
		},
		LsRemote: func(dir, branch string) (string, error) {
			out, err := submitGit(dir, "ls-remote", "origin", "refs/heads/"+branch)
			if err != nil {
				return "", err
			}
			fields := strings.Fields(out)
			if len(fields) == 0 {
				return "", nil
			}
			return fields[0], nil
		},
		DeleteLocalBranch: func(dir, branch string) error {
			if _, err := submitGit(dir, "checkout", "--detach"); err != nil {
				return err
			}
			_, err := submitGit(dir, "branch", "-D", branch)
			return err
		},
		Wake: func(target string) error {
			if cmdSessionWake([]string{target}, io.Discard, stderr) != 0 {
				return errors.New("gc session wake failed")
			}
			return nil
		},
		Nudge: func(target, message string) error {
			if cmdSessionNudge([]string{target, message}, nudgeDeliveryWaitIdle, false, io.Discard, stderr) != 0 {
				return errors.New("gc session nudge failed")
			}
			return nil
		},
	}
	return doSubmit(store, opts, ops, stderr)
}

// doSubmit is the fail-closed core. Every refusal returns before any store
// write; the only writes are the auto_push=false halt and the final handoff,
// and the handoff write is reachable solely through a successful ls-remote
// match against the local HEAD.
func doSubmit(store beads.Store, opts submitOptions, ops submitOps, stderr io.Writer) (submitResult, error) {
	if strings.TrimSpace(opts.Refinery) == "" {
		return submitResult{}, errors.New("--refinery is required")
	}
	base := strings.TrimSpace(opts.BaseBranch)
	if base == "" {
		return submitResult{}, errors.New("--base is required")
	}

	beadID, err := submitResolveBeadID(store, opts, ops)
	if err != nil {
		return submitResult{}, err
	}
	bead, err := store.Get(beadID)
	if err != nil {
		return submitResult{}, fmt.Errorf("reading work bead %s: %w", beadID, err)
	}
	// Review gate (sys-pxryan.50): applies ONLY to review_required work. Read
	// HEAD once, then refuse before any push or store write unless the owning
	// session's claimed verified-submit step has a settled approved review for
	// exactly this HEAD; head is carried into the LocalSHA assignment below.
	// Legacy work (no review_required) keeps the pre-gate flow byte-identical:
	// no HEAD read here. It is read later, exactly where the original code read
	// it (after the ahead==0 check), so HEAD reads and error order are unchanged.
	var head string
	if strings.EqualFold(strings.TrimSpace(bead.Metadata["review_required"]), "true") {
		head, err = ops.Head(opts.Dir)
		if err != nil {
			return submitResult{}, fmt.Errorf("reading HEAD for review gate: %w", err)
		}
		if err := reviewSubmitGuard(store, bead, head); err != nil {
			return submitResult{}, err
		}
	}

	if !ops.RefineryConfigured(opts.Refinery) {
		return submitResult{}, fmt.Errorf("refinery target %q does not name a configured agent; refusing to hand off (the bead would be stranded)", opts.Refinery)
	}

	expected := strings.TrimSpace(bead.Metadata["branch"])
	if expected == "" {
		expected = "polecat/" + beadID
	}
	if expected == base {
		return submitResult{}, fmt.Errorf("work branch %q is the base branch; refusing to hand off from the base branch", expected)
	}
	res := submitResult{BeadID: beadID, Branch: expected, Target: base, Refinery: opts.Refinery}

	current, err := ops.CurrentBranch(opts.Dir)
	if err != nil {
		return submitResult{}, fmt.Errorf("reading current branch: %w", err)
	}
	if current == "" {
		return submitResult{}, fmt.Errorf("HEAD is detached; the refinery merges %s, so check that branch out (or recreate it from origin/%s and cherry-pick) before submitting", expected, base)
	}
	if current != expected {
		return submitResult{}, fmt.Errorf("HEAD is on %q but the refinery merges %q (metadata.branch, else polecat/<bead>); refusing to hand off from another branch", current, expected)
	}

	clean, err := ops.IsClean(opts.Dir)
	if err != nil {
		return submitResult{}, fmt.Errorf("checking working tree: %w", err)
	}
	if !clean {
		return submitResult{}, errors.New("working tree has uncommitted or untracked changes; commit or discard them, then submit again")
	}

	if strings.EqualFold(strings.TrimSpace(bead.Metadata["auto_push"]), "false") {
		res.Halted = true
		res.HaltReason = submitHaltReasonAutoPushFalse
		statusOpen, noAssignee := "open", ""
		if err := store.Update(beadID, beads.UpdateOpts{
			Status:   &statusOpen,
			Assignee: &noAssignee,
			Metadata: map[string]string{
				"branch":       expected,
				"target":       base,
				"branch_ready": "true",
				"halt_reason":  submitHaltReasonAutoPushFalse,
				"gc.routed_to": "",
			},
		}); err != nil {
			return submitResult{}, fmt.Errorf("recording branch-ready halt on %s: %w", beadID, err)
		}
		return res, nil
	}

	if err := ops.FetchBase(opts.Dir, base); err != nil {
		return submitResult{}, fmt.Errorf("fetching origin/%s: %w", base, err)
	}
	ahead, err := ops.CommitsBeyond(opts.Dir, base)
	if err != nil {
		return submitResult{}, fmt.Errorf("counting commits beyond origin/%s: %w", base, err)
	}
	if ahead == 0 {
		return submitResult{}, fmt.Errorf("%s has no commits beyond origin/%s; nothing to hand off", expected, base)
	}
	if head == "" {
		// Legacy path: the review gate did not read HEAD early, so read it here
		// exactly as the pre-gate code did (same position, same error text).
		if head, err = ops.Head(opts.Dir); err != nil {
			return submitResult{}, fmt.Errorf("reading HEAD: %w", err)
		}
	}
	res.LocalSHA = head

	if err := ops.Push(opts.Dir, expected); err != nil {
		return submitResult{}, fmt.Errorf("push of %s to origin failed, bead stays with the polecat: %w", expected, err)
	}
	remote, err := ops.LsRemote(opts.Dir, expected)
	if err != nil {
		return submitResult{}, fmt.Errorf("verifying %s on origin after push: %w", expected, err)
	}
	if remote == "" {
		return submitResult{}, fmt.Errorf("push reported success but origin has no ref for %s; branch is local-only, refusing to hand off", expected)
	}
	if remote != head {
		return submitResult{}, fmt.Errorf("origin/%s is at %s but local HEAD is %s; refusing to hand off an unverified tip", expected, remote, head)
	}
	res.RemoteSHA = remote

	// The ONLY path to this write is a successful ls-remote match above.
	statusOpen := "open"
	refinery := opts.Refinery
	if err := store.Update(beadID, beads.UpdateOpts{
		Status:   &statusOpen,
		Assignee: &refinery,
		Metadata: map[string]string{
			"branch":       expected,
			"target":       base,
			"gc.routed_to": "",
		},
	}); err != nil {
		return submitResult{}, fmt.Errorf("branch %s is verified on origin at %s but recording the handoff on %s failed: %w", expected, remote, beadID, err)
	}
	res.HandedOff = true

	if err := ops.DeleteLocalBranch(opts.Dir, expected); err != nil {
		fmt.Fprintf(stderr, "gc submit: warning: could not delete local branch %s after handoff: %v\n", expected, err) //nolint:errcheck
	}
	if err := ops.Wake(opts.Refinery); err != nil {
		fmt.Fprintf(stderr, "gc submit: warning: wake %s: %v (refinery finds the work on its next poll)\n", opts.Refinery, err) //nolint:errcheck
	}
	if err := ops.Nudge(opts.Refinery, "Run 'gc prime' to check merge queue and begin processing."); err != nil {
		fmt.Fprintf(stderr, "gc submit: warning: nudge %s: %v (refinery finds the work on its next poll)\n", opts.Refinery, err) //nolint:errcheck
	}
	return res, nil
}

// reviewSubmitGuard enforces the review handoff gate at the submit seam
// (sys-pxryan.27, sys-pxryan.50): a work bead stamped review_required=true may
// only be handed to the refinery by the session whose persisted claim IS the
// live verified-submit step, and only while that workflow's terminal review
// loop has a settled approved review for the exact candidate SHA about to be
// pushed.
//
// The submitted --bead/--convoy target is the WORK bead, NOT the current
// execution step. GC_BEAD_ID is a non-authoritative hint only: the formula
// itself exports it as the workflow root when it invokes the checker, so it
// can name the step or the root depending on the caller and must never be
// trusted alone. Authority is the authenticated hook/claim stamp: gc.session_id
// / gc.session_name on the step (written by stampHookClaimIdentity at claim
// time), cross-checked against the step's own gc.root_bead_id and gc.step_ref.
// resolveActiveWispStep is deliberately NOT used to pick the step: its
// entry-step fallback returns the oldest OPEN child with no ownership check, so
// it cannot authenticate an OPEN non-entry verified-submit step.
//
// Every refusal returns before any push or store write. Targets without
// review_required=true keep the legacy manual/polecat path unchanged; the
// Refinery's own review gate remains defense in depth.
func reviewSubmitGuard(store beads.Store, work beads.Bead, candidateSHA string) error {
	if !strings.EqualFold(strings.TrimSpace(work.Metadata["review_required"]), "true") {
		return nil // legacy/manual path: unchanged
	}
	sid := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	sname := strings.TrimSpace(os.Getenv("GC_SESSION_NAME"))
	if sid == "" && sname == "" {
		return fmt.Errorf("gc submit: %s is review_required=true but this invocation carries no session identity (GC_SESSION_ID/GC_SESSION_NAME both empty); only the owning verified-submit session may hand it to the refinery (sys-pxryan.50)", work.ID)
	}
	// Bind the work bead to its workflow root.
	root := strings.TrimSpace(work.Metadata["gc.dispatch_workflow"])
	if root == "" {
		root = strings.TrimSpace(work.Metadata["gc.root_bead_id"])
	}
	if root == "" {
		return fmt.Errorf("gc submit: %s is review_required=true but has no workflow root (gc.dispatch_workflow/gc.root_bead_id empty); refusing before push or handoff write (sys-pxryan.50)", work.ID)
	}
	// Step provenance: the caller's claimed current step must be exactly the
	// verified-submit step under this root.
	if err := requireClaimedVerifiedSubmitStep(store, sid, sname, root, work.ID); err != nil {
		return err
	}
	// Review binding: the terminal closed/pass ralph control's settled attempt
	// must carry a native review approval of exactly the candidate SHA.
	return assertSettledReview(store, root, candidateSHA)
}

// requireClaimedVerifiedSubmitStep authenticates the caller's current execution
// step from persisted claim state. Among non-closed native steps (gc.step_ref
// set - work beads and cards are not steps), the ones stamped to THIS exact
// session (gc.session_id / gc.session_name, written by the authenticated
// hook/claim path; the stamp is the only authority - assignee alone never
// authorizes) must be EXACTLY ONE, and it must be the verified-submit step
// under root. A preassigned-but-unstamped step is not claim proof. An earlier
// OPEN step owned by ANOTHER session is simply not in the set; multiple
// claimed steps (e.g. an apply step plus verified-submit) are ambiguous and
// fail closed. sid/sname are the exact claim identity - no fuzzy alias
// expansion.
func requireClaimedVerifiedSubmitStep(store beads.Store, sid, sname, root, workID string) error {
	steps, err := store.List(beads.ListQuery{
		TierMode:      beads.TierBoth,
		IncludeClosed: false, // non-closed only: open + in_progress
		AllowScan:     true,  // unfiltered: ownership decided in memory
	})
	if err != nil {
		return fmt.Errorf("gc submit: %s: listing open steps for session claim check: %w (sys-pxryan.50)", workID, err)
	}
	claimed := 0
	claimedVerifiedSubmit := false
	for _, b := range steps {
		if strings.TrimSpace(b.Metadata["gc.step_ref"]) == "" {
			continue
		}
		if !stepClaimedBySession(b, sid, sname) {
			continue
		}
		claimed++
		if strings.TrimSpace(b.Metadata["gc.root_bead_id"]) == root && isVerifiedSubmitRef(b.Metadata["gc.step_ref"]) {
			claimedVerifiedSubmit = true
		}
	}
	switch {
	case claimed == 0:
		return fmt.Errorf("gc submit: %s is review_required=true but no open step is claimed by this session; the verified-submit step must be claimed by its owning session before handoff (sys-pxryan.50)", workID)
	case claimed > 1:
		return fmt.Errorf("gc submit: %s: this session claims %d open steps, so the current step is ambiguous; refusing (sys-pxryan.50)", workID, claimed)
	case !claimedVerifiedSubmit:
		return fmt.Errorf("gc submit: %s: this session's current step is not the verified-submit step of workflow %s; only verified-submit may hand off review_required work (sys-pxryan.50)", workID, root)
	}
	return nil
}

// stepClaimedBySession reports whether a step carries THIS session's exact
// hook-claim stamp (gc.session_id / gc.session_name, written by
// stampHookClaimIdentity at claim time). Assignee is deliberately NOT
// consulted: a preassigned-but-unstamped step is not a claim, and any stamp
// field naming a different session is a conflict - both are rejected even if
// the assignee matches. At least one stamp field must positively identify this
// session.
func stepClaimedBySession(b beads.Bead, sid, sname string) bool {
	stepSID := strings.TrimSpace(b.Metadata["gc.session_id"])
	stepSname := strings.TrimSpace(b.Metadata["gc.session_name"])
	if stepSID == "" && stepSname == "" {
		return false // never claimed by a session; assignee is not authority here
	}
	// Any stamp field present must name this session; a field naming a
	// different session is a conflict -> fail closed.
	if stepSID != "" && stepSID != sid {
		return false
	}
	if stepSname != "" && stepSname != sname {
		return false
	}
	return (sid != "" && stepSID == sid) || (sname != "" && stepSname == sname)
}

// assertSettledReview binds the approval to the loop's SETTLED attempt instead
// of max-scanning attempt numbers over arbitrary descendants:
//
//  1. The terminal ralph control under root (gc.kind=ralph, closed,
//     gc.outcome=pass) must exist and be unique.
//  2. Its gc.attempt_log (the JSON array appendAttemptLogValue writes)
//     settles the attempt - the last entry's attempt.
//  3. That iteration's NATIVE review member must exist and be unique:
//     gc.ralph_step_id == the control's step id, gc.step_ref ending
//     ".<loop>.review" (scope-check beads end "-scope-check" and never match),
//     gc.attempt == the settled attempt, closed with gc.outcome=pass.
//  4. That member must carry review.verdict=approve and
//     review.reviewed_sha == candidateSHA.
//
// apply / scope / control descendants copy approve and SHA fields (observed in
// production roots), so they are never accepted as review proof: only the
// native member at the settled attempt can approve. A stale-attempt approval of
// the SAME SHA (an earlier iteration) is refused by the attempt binding.
func assertSettledReview(store beads.Store, root, candidateSHA string) error {
	if strings.TrimSpace(candidateSHA) == "" {
		return fmt.Errorf("gc submit: review gate cannot verify approval with an empty candidate HEAD; refusing (sys-pxryan.50)")
	}
	all, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{"gc.root_bead_id": root},
		IncludeClosed: true, // the loop is terminal (closed) by submit time
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return fmt.Errorf("gc submit: listing review beads under workflow %s: %w (sys-pxryan.50)", root, err)
	}
	var under []beads.Bead
	var control *beads.Bead
	for i := range all {
		b := &all[i]
		// Belt: verify the root in memory (backend-agnostic filter check).
		if strings.TrimSpace(b.Metadata["gc.root_bead_id"]) != root {
			continue
		}
		under = append(under, *b)
		if strings.TrimSpace(b.Metadata["gc.kind"]) == "ralph" && b.Status == "closed" &&
			strings.TrimSpace(b.Metadata["gc.outcome"]) == "pass" {
			if control != nil {
				return fmt.Errorf("gc submit: workflow %s has multiple terminal ralph controls; refusing (sys-pxryan.50)", root)
			}
			control = b
		}
	}
	if control == nil {
		return fmt.Errorf("gc submit: workflow %s has no terminal closed/pass ralph control; the review loop never settled, refusing to hand off review_required work (sys-pxryan.50)", root)
	}
	// Settled attempt = last entry of the control's gc.attempt_log (the same
	// JSON shape appendAttemptLogValue writes).
	var log []map[string]string
	if raw := strings.TrimSpace(control.Metadata["gc.attempt_log"]); raw != "" {
		if uerr := json.Unmarshal([]byte(raw), &log); uerr != nil {
			return fmt.Errorf("gc submit: ralph control %s has a malformed gc.attempt_log: %v; refusing (sys-pxryan.50)", control.ID, uerr)
		}
	}
	settled := ""
	if len(log) > 0 {
		settled = strings.TrimSpace(log[len(log)-1]["attempt"])
	}
	if settled == "" {
		return fmt.Errorf("gc submit: ralph control %s has no settled attempt in gc.attempt_log; refusing (sys-pxryan.50)", control.ID)
	}
	loopID := strings.TrimSpace(control.Metadata["gc.step_id"])
	if loopID == "" {
		return fmt.Errorf("gc submit: ralph control %s has no gc.step_id; cannot identify its review member (sys-pxryan.50)", control.ID)
	}
	suffix := "." + loopID + ".review"
	member := 0
	var verdict, reviewedSHA string
	for _, b := range under {
		if strings.TrimSpace(b.Metadata["gc.ralph_step_id"]) != loopID {
			continue
		}
		if strings.TrimSpace(b.Metadata["gc.kind"]) == "scope-check" {
			continue // checker beads copy approve/SHA fields; not review proof
		}
		if !strings.HasSuffix(strings.TrimSpace(b.Metadata["gc.step_ref"]), suffix) {
			continue // apply ("-apply") and scope-check ("-scope-check") never match
		}
		if strings.TrimSpace(b.Metadata["gc.attempt"]) != settled {
			continue // stale-attempt approval (same or different SHA) must not bind
		}
		if b.Status != "closed" || strings.TrimSpace(b.Metadata["gc.outcome"]) != "pass" {
			continue
		}
		member++
		verdict = strings.TrimSpace(b.Metadata["review.verdict"])
		reviewedSHA = strings.TrimSpace(b.Metadata["review.reviewed_sha"])
	}
	switch {
	case member == 0:
		return fmt.Errorf("gc submit: workflow %s has no native review member for settled attempt %s (loop %s); no independent approval exists, refusing (sys-pxryan.50)", root, settled, loopID)
	case member > 1:
		return fmt.Errorf("gc submit: workflow %s has %d native review members for settled attempt %s; refusing (sys-pxryan.50)", root, member, settled)
	case verdict != "approve":
		return fmt.Errorf("gc submit: workflow %s attempt %s has no review.verdict=approve (got %q); refusing to hand off review_required work to the refinery before an approval exists (sys-pxryan.50)", root, settled, verdict)
	case reviewedSHA != candidateSHA:
		return fmt.Errorf("gc submit: workflow %s attempt %s reviewed_sha=%q does not match the candidate SHA %s being pushed (stale or missing approval); refusing (sys-pxryan.50)", root, settled, reviewedSHA, candidateSHA)
	}
	return nil
}

// isVerifiedSubmitRef reports whether a gc.step_ref identifies the native
// verified-submit step, in any formula and at any iteration depth
// (e.g. "homeops-work-reviewed.verified-submit").
func isVerifiedSubmitRef(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	parts := strings.Split(ref, ".")
	return parts[len(parts)-1] == "verified-submit"
}

func submitResolveBeadID(store beads.Store, opts submitOptions, ops submitOps) (string, error) {
	beadID := strings.TrimSpace(opts.BeadID)
	convoyID := strings.TrimSpace(opts.ConvoyID)
	switch {
	case beadID != "" && convoyID != "":
		return "", errors.New("pass --bead or --convoy, not both")
	case beadID != "":
		return beadID, nil
	case convoyID == "":
		return "", errors.New("--bead or --convoy is required")
	}
	members, err := ops.ConvoyChildren(store, convoyID)
	if err != nil {
		return "", fmt.Errorf("listing convoy %s members: %w", convoyID, err)
	}
	if len(members) != 1 {
		return "", fmt.Errorf("convoy %s has %d open members; pass --bead to name the work bead", convoyID, len(members))
	}
	return members[0].ID, nil
}

func submitGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitpkg.SanitizedEnv()
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, text)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return text, nil
}
