# Release Gate: ga-pjqjrm hook route filter

Bead: `ga-pjqjrm` - needs-deploy: gc hook display route-filter via hookClaimMatchesRoute

Branch: `release/ga-pjqjrm-hook-route-filter`

Reviewed commit: `c6aad5e8f3634bec3305d828f8ab6afea21e7794` (rebased equivalent: `684872d3366df3458ad86387a5c794c25b4bad3b`)

Base: `origin/main` at `d9225156f9b30fa4fe7fee28c30699ee2cc8cc3d` (rebased onto `9b6d91e1755ab968a66c7b30a53be9a626c17c9b`, rebase gate: 2026-07-13)

Reviewer bead: `ga-hksfp1`

Project manifest note: `docs/PROJECT_MANIFEST.md` is not present in this checkout (`rg --files -g '*PROJECT_MANIFEST*'` returned no manifest). Gate criteria below use the active deployer prompt criteria and `TESTING.md` sharded-runner guidance.

## Summary

This is a single-bead deploy for the `gc hook` display route-filter change. The branch is one feature commit on top of `origin/main`. It changes one subsystem: hook display visibility and the config/API/schema support for the `work_query_unfiltered` opt-out.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | Reviewer bead `ga-hksfp1` is closed with close reason `pass` and notes contain `Review verdict: PASS (gascity/reviewer)`. |
| 2 | Acceptance criteria met | PASS | The reviewed diff implements the ratified `ga-2rpi53` Option A predicate: keep display candidates assigned to the current identity or matching `hookClaimMatchesRoute`, with `WorkQueryUnfiltered` as the explicit opt-out. The diff includes hook display tests, config field-sync coverage, migration coverage, and regenerated schema/API artifacts. |
| 3 | Tests pass | PASS | Rebase re-verification (2026-07-13): first `TMPDIR=/var/tmp/gp make test-fast-parallel` post-rebase hit 5 failures (`TestCityRuntimeReloadDrainBoundedByTimeout`, `TestCmdStopForceDelegatesImmediateControllerStop`, `TestCmdStopForceEscalatesInProgressControllerStop`, `TestCmdStopMarginExhaustion`, `TestHandleExtMsgInboundDefaultRouteMatchesMixedCaseProvider`) — all timeout/tmpdir-cleanup/wall-clock-margin signatures consistent with 6-way parallel shard contention, and none in files this PR changes. Isolated re-runs with no parallel contention confirmed pre-existing flakiness, not a regression: the three `TestCmdStop*` tests passed 9/9 across 3 runs each, `TestHandleExtMsgInboundDefaultRouteMatchesMixedCaseProvider` passed 5/5, `TestCityRuntimeReloadDrainBoundedByTimeout` passed 2/3 (itself a load-sensitive wall-clock bound). The pre-push hook's own fresh full-suite dry-run (triggered by the force-push below) then passed all 8 fast jobs cleanly. `go vet ./...` clean. `make dashboard-check` PASS. `make check-schema` and `make spec-ci` both clean (no artifact drift). See Test Details below. |
| 4 | No high-severity review findings open | PASS | Reviewer notes state "No blocking findings. Routing to deployer." No unresolved HIGH findings were recorded in the deploy or review bead notes. |
| 5 | Final branch is clean | PASS | `git status --short --branch` reports no uncommitted changes after rebase (only pre-existing untracked worktree-shared `.claude/skills/` entries, unrelated to this branch). Force-pushed rebased branch to `origin release/ga-pjqjrm-hook-route-filter`. |
| 6 | Branch diverges cleanly from main | PASS | After rebase onto `origin/main` at `9b6d91e1755ab968a66c7b30a53be9a626c17c9b`, `git merge-base --is-ancestor origin/main HEAD` passes. `gh pr view 3952` reports `mergeable: MERGEABLE` (no content conflict). `mergeStateStatus: BLOCKED` at push time reflects CI checks freshly kicked off by the force-push and no native GitHub review recorded (review is tracked via bead `ga-hksfp1`, not a native GH approval) — not a content-conflict block. |
| 7 | Single feature theme | PASS | `git cherry -v origin/main HEAD` shows exactly 2 commits unique to the branch: the feature commit `684872d3366df3458ad86387a5c794c25b4bad3b` (rebased equivalent of reviewed `c6aad5e8f`) and this gate doc's own PASS commit `0bb71e62b`. Diff-stat against `origin/main` touches exactly the same 19 files as the "Changed Files Reviewed For Scope" list below, plus this gate doc. No independent feature theme is bundled. |

## Acceptance Checks

- `gc hook` display now route-filters the no-`--claim` path through the same identity and route predicates used by claim logic.
- Assigned-to-self work remains visible even when `gc.routed_to` does not match the current route, preserving Tier-1 crash recovery behavior.
- Unrouted workflow-root candidates for the current run target remain visible.
- `work_query_unfiltered` opts out intentionally cross-cutting custom work queries without changing `--claim`.
- `config.Agent`, `AgentPatch`, `AgentOverride`, apply functions, pool deep copy, migration structs, OpenAPI, generated client, and reference schemas were updated together.

## Test Details

- `TMPDIR=/var/tmp/gp make test-fast-parallel`: PASS, all fast jobs passed.
- `TMPDIR=/var/tmp/gascity-deploy-ga-pjqjrm-tmp go vet ./...`: PASS.
- `TMPDIR=/var/tmp/gascity-deploy-ga-pjqjrm-tmp make dashboard-check`: PASS.
- Dashboard preview smoke: PASS, Vite preview served the built app at `http://127.0.0.1:4792/`.
- Initial `TMPDIR=/var/tmp/gascity-deploy-ga-pjqjrm-tmp make test-fast-parallel` attempt: FAIL due to generated Unix socket paths under the long temp prefix returning `bind: invalid argument`. Re-run with the short temp root above passed.

### Rebase re-verification (2026-07-13)

- `TMPDIR=/var/tmp/gp make test-fast-parallel` (first run post-rebase): 5 failures (`TestCityRuntimeReloadDrainBoundedByTimeout`, `TestCmdStopForceDelegatesImmediateControllerStop`, `TestCmdStopForceEscalatesInProgressControllerStop`, `TestCmdStopMarginExhaustion`, `TestHandleExtMsgInboundDefaultRouteMatchesMixedCaseProvider`), all in subsystems unrelated to this PR's changed files.
- Isolated re-runs (no parallel shard contention): `go test ./cmd/gc/... -run 'TestCityRuntimeReloadDrainBoundedByTimeout|TestCmdStopForceDelegatesImmediateControllerStop|TestCmdStopForceEscalatesInProgressControllerStop|TestCmdStopMarginExhaustion' -count=3` — the three `TestCmdStop*` tests passed 9/9; `TestCityRuntimeReloadDrainBoundedByTimeout` passed 2/3 (a wall-clock margin check that is itself load-sensitive). `go test ./internal/api/... -run TestHandleExtMsgInboundDefaultRouteMatchesMixedCaseProvider -count=5` passed 5/5. Conclusion: pre-existing environmental flakes from 6-way parallel resource contention, not regressions from this rebase.
- `go vet ./...`: PASS.
- `TMPDIR=/var/tmp/gp make dashboard-check`: PASS (frontend build, typecheck, typecheck:test, and `go test ./internal/api/dashboardspa/... ./internal/api/dashboardbff/...` all passed).
- `make check-schema`: PASS, no drift.
- `make spec-ci`: PASS, no drift in OpenAPI/client artifacts.
- Pre-push hook's own fresh full-suite dry-run (triggered by the force-push): all 8 fast jobs passed cleanly, including the tests that had failed above.

## Changed Files Reviewed For Scope

```text
cmd/gc/cmd_hook.go
cmd/gc/cmd_hook_test.go
cmd/gc/pool.go
cmd/gc/pool_test.go
docs/reference/config.md
docs/reference/schema/city-schema.json
docs/reference/schema/city-schema.txt
docs/reference/schema/openapi.json
docs/reference/schema/openapi.txt
docs/reference/schema/pack-schema.json
docs/reference/schema/pack-schema.txt
internal/api/genclient/client_gen.go
internal/api/openapi.json
internal/config/config.go
internal/config/field_sync_test.go
internal/config/pack.go
internal/config/patch.go
internal/migrate/migrate.go
internal/migrate/migrate_test.go
```

## Rebase resolution (2026-07-13)

Builder rebased `release/ga-pjqjrm-hook-route-filter` onto `origin/main` at
`9b6d91e1755ab968a66c7b30a53be9a626c17c9b` (148 commits ahead of the prior
base) after the PR went stale and reported CONFLICTING for 7 days.

One conflict, in `internal/config/pack.go`: main had refactored the
field-by-field `applyAgentOverride`/`applyAgentPatchFields` pattern into a
single shared `applyAgentMutation(a, p, sleepSource)` body fed by a new
`toAgentPatch()` adapter that converts `AgentOverride` into `AgentPatch`,
while this branch's reviewed commit still targeted the pre-refactor
field-by-field style to add the `WorkQueryUnfiltered` override. Resolved by
keeping main's refactored structure and threading this branch's one new
field through the adapter: added `WorkQueryUnfiltered: ov.WorkQueryUnfiltered,`
to the `toAgentPatch()` struct literal, matching `AgentPatch`'s field order.
Verified correct against `internal/config/patch.go`, which auto-merged
cleanly and already handles `WorkQueryUnfiltered` inside
`applyAgentMutation` — confirming the adapter now wires the override path
end-to-end. `internal/config/config.go`'s `AgentOverride.WorkQueryUnfiltered`
and `Agent.WorkQueryUnfiltered` struct fields also auto-merged cleanly with
no manual resolution needed.

Post-rebase re-verification is recorded in criteria 3, 5, 6, 7 above and in
the Rebase re-verification test details above. Force-pushed the rebased
branch to `origin release/ga-pjqjrm-hook-route-filter`; `gh pr view 3952`
now reports `mergeable: MERGEABLE`.

## Decision

PASS. All 7 criteria hold on the rebased branch. PR #3952 is clean against
current `origin/main` with no content conflicts. Route merge-request to
mayor/mpr.

Gate result: PASS (re-confirmed after rebase, 2026-07-13).
