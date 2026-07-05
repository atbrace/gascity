# Release Gate: ga-pjqjrm hook route filter

Bead: `ga-pjqjrm` - needs-deploy: gc hook display route-filter via hookClaimMatchesRoute

Branch: `release/ga-pjqjrm-hook-route-filter`

Reviewed commit: `c6aad5e8f3634bec3305d828f8ab6afea21e7794`

Base: `origin/main` at `d9225156f9b30fa4fe7fee28c30699ee2cc8cc3d`

Reviewer bead: `ga-hksfp1`

Project manifest note: `docs/PROJECT_MANIFEST.md` is not present in this checkout (`rg --files -g '*PROJECT_MANIFEST*'` returned no manifest). Gate criteria below use the active deployer prompt criteria and `TESTING.md` sharded-runner guidance.

## Summary

This is a single-bead deploy for the `gc hook` display route-filter change. The branch is one feature commit on top of `origin/main`. It changes one subsystem: hook display visibility and the config/API/schema support for the `work_query_unfiltered` opt-out.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | Reviewer bead `ga-hksfp1` is closed with close reason `pass` and notes contain `Review verdict: PASS (gascity/reviewer)`. |
| 2 | Acceptance criteria met | PASS | The reviewed diff implements the ratified `ga-2rpi53` Option A predicate: keep display candidates assigned to the current identity or matching `hookClaimMatchesRoute`, with `WorkQueryUnfiltered` as the explicit opt-out. The diff includes hook display tests, config field-sync coverage, migration coverage, and regenerated schema/API artifacts. |
| 3 | Tests pass | PASS | `TMPDIR=/var/tmp/gp make test-fast-parallel` passed all 8 fast jobs. `go vet ./...` passed. `make dashboard-check` passed. Dashboard preview served successfully from `npm --workspace gas-city-dashboard-frontend run preview -- --host 127.0.0.1 --port 4792`, verified with `curl -fsS http://127.0.0.1:4792/`. An extra pre-push dry-run hook also ran the fast suite and passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes state "No blocking findings. Routing to deployer." No unresolved HIGH findings were recorded in the deploy or review bead notes. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before writing this gate file; the final deployment commit contains only this gate file on top of the reviewed commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` passed before the gate commit. The reviewed branch is a direct one-commit descendant of `origin/main`. |
| 7 | Single feature theme | PASS | The commit set is one coherent hook/config feature: `cmd/gc` hook display filtering plus the required config, migration, API, schema, and docs/reference artifacts for `work_query_unfiltered`. No independent feature theme is bundled. |

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

Gate result: PASS.
