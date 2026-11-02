# 0007. Change-based CI with one required check

- Status: Accepted. In effect from 2026-10-10: `master` requires `ci-ok`.
- Date: 2026-10-10. Amended 2026-10-26: the release pull request also runs
  every PatientZero scenario (`scenarios.yml`, about two and a half hours),
  chosen by its branch rather than its paths, and `ci-ok` includes it; the
  same workflow runs nightly on `master` and by hand. Ordinary pull requests
  keep their change-based jobs. Amended 2026-11-01: `test` runs the Go tests
  once, with the race detector, not once with and once without it.

## Context

CI ran every job on every pull request: Go tests and checks (about 3
minutes, 80 seconds of it the SQL Server probe tests), the chart checks
(40 seconds) and the acceptance checks on a temporary kind cluster (about 9
minutes). A pull request that changed only docs or `TODO.md` waited about 10
minutes for checks its change could not affect.

The next phases add services (OTel gateway, API, web UI), each with its own
slower tests, so running everything on every change gets more expensive.

[ADR 0006](0006-trunk-based-development-and-tagged-releases.md) made `test`,
`Helm chart` and `kind end-to-end` required checks on `master`. Skipping jobs
interacts badly with required checks:

- A workflow filtered out by `paths:` never starts, so its checks are never
  reported, and a pull request that requires them waits forever.
- GitHub counts a skipped job as a passed check. A required job skipped by
  mistake, by a wrong condition or because a job it depends on failed, lets an
  untested change merge.

## Decision

| Concern | Choice | Why |
| --- | --- | --- |
| Workflow | One workflow, `.github/workflows/ci.yml` (was `db-collector-ci.yml`), started by every pull request, every push to `master` and by hand. No `paths:` filters on its triggers | Every check is always reported; the old name no longer matched what it tests. |
| What always runs | Go tests with the race detector and vet, Prometheus rules, the docs site, the PostgreSQL integration tests (`test`, about 2 minutes) | They are cheap and catch breakage in shared code that a path rule might miss. |
| What runs on change | A `changes` job lists the paths the pull request changes and [`scripts/ci-changes.sh`](../../../scripts/ci-changes.sh) turns them into flags: SQL Server probe tests (`db_collector`), chart checks (`chart`), kind acceptance checks (`kind_e2e`). Each slow job runs only when its flag is true | The slow jobs are where the time goes. |
| Unknown paths | Any path the script does not list (go.mod, `internal/`, the config schema, the Makefile, workflows, new directories) runs every job | A missing rule costs minutes, not an untested merge. |
| Full runs | Pushes to `master` and manual runs (Actions → ci → Run workflow) run every job | Every commit a release can tag passed the full suite; a rule that skipped too much is caught before a release. A manual run on a branch is the way to force a full run on its pull request. |
| Required check | **`ci-ok` only.** It always runs and [`scripts/ci-gate.sh`](../../../scripts/ci-gate.sh) fails it unless every job did what the flags asked: the always-run jobs succeeded, each flagged job succeeded, and each unflagged job was skipped. It also fails if a job is missing from its list | One check that cannot pass by being skipped, and that need not change when jobs are added. |
| Release verification | `scripts/release-verify.sh` waits for `ci-ok` on the tagged commit; commits from before `ci.yml` keep the three checks they ran | The check name changed; old tags such as `v0.1.0` stay releasable. |
| Detection tool | A shell script over `git diff --name-only --no-renames` of the pull request's merge commit, tested by `tests/ci_scripts_test.go` | No third-party action to pin and trust; testable locally; `--no-renames` counts a file moved out of a component as a change to it. |

Considered and not chosen:

- **A workflow per component with `paths:` filters.** The usual first attempt,
  and the one that leaves required checks waiting forever.
- **Requiring every job.** A skipped job reports success, so it adds nothing
  over `ci-ok` and has to be edited for every new job.
- **`dorny/paths-filter`.** Works well, but it is a third-party action with
  repository access, and the rules would live in YAML with no test.
- **Precise detection from `go list -deps`.** More exact for Go, but the
  services share most code today, so it would rarely skip more.

## Consequences

- A docs-only or `TODO.md`-only pull request takes about 2 minutes instead of
  about 10. A change to `internal/`, `go.mod` or the config schema still runs
  everything, which is most of phase 1's history.
- Adding a job: give it `needs: changes` and an `if:` on a flag, add the flag
  to `scripts/ci-changes.sh` and its test, and add the job to `ci-ok`'s
  `needs` and arguments. The gate fails until all three agree.
- When a new directory appears it runs everything until a rule is added for it.
- A new push to a pull request cancels its older run; runs on `master` are
  never cancelled.
- The `master` ruleset must list `ci-ok` as its required check, a manual
  change in the repository settings.
