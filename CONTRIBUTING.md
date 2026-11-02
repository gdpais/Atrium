# Contributing to Heartbeat

How to branch, commit, test and open pull requests, for people and coding
agents alike. The reasoning behind these rules is in
[ADR 0006](docs/architecture/decisions/0006-trunk-based-development-and-tagged-releases.md).

## Workflow at a glance

1. Pick an item from [TODO.md](TODO.md) whose dependencies are done.
2. Branch from an up-to-date `master`: `feature/<todo#>-<slug>`.
3. Commit with Conventional Commits; keep tests and docs in the same branch.
4. Merge the latest `master` in, run the checks, open a pull request to
   `master`.
5. CI passes, the pull request is merged with a merge commit, and the branch is
   deleted.

`master` is protected: every change arrives by pull request and must pass CI.

## Branches

Name branches `feature/<todo#>-<slug>`, with the TODO section number and a
few words, for example `feature/2.7-endpoint-security` or
`feature/3.3-outsystems-parsers`. Work without a TODO item uses
`feature/<slug>`.

```bash
git switch master && git pull
git switch -c feature/3.3-outsystems-parsers
```

Keep branches short-lived: days, not weeks. A large item lands in several
pull requests. Unfinished parts are merged switched off: not wired in yet, or
behind a configuration flag. Every commit on `master` must be safe to release.

Database schema changes follow expand, then contract: add the new column or
table, move the code over, and remove the old one in a later pull request. A
rollback does not undo a migration.

## Working in parallel

Several people or agents can work at once, one branch each, in separate
worktrees:

```bash
git worktree add .claude/worktrees/3.3-outsystems-parsers -b feature/3.3-outsystems-parsers master
```

- Name the worktree after its branch, never a random name, so it is clear what
  it holds. `.claude/worktrees/` is ignored by git; a folder outside the
  repository also works.
- Run in parallel only items that touch different code. Items that change the
  same files, or depend on each other, go one after another.
- Before opening a pull request, and again if `master` moves while it is open,
  merge `master` into the branch and re-run the checks:
  `git fetch && git merge origin/master`. Never rebase or force-push a branch
  that has been pushed.
- When the pull request is merged, remove the worktree with
  `git worktree remove <path>` and delete the local branch with
  `git branch -d <branch>`.

## Commits

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/).
The changelog and the next version number are generated from them, so a
commit that does not follow the format is left out of the release notes.

```text
<type>(<scope>): <what changed, imperative, lower case>
```

| Type | Use for | Version bump |
| --- | --- | --- |
| `feat` | New behavior a user or operator can see | minor |
| `fix` | A bug fix | patch |
| `perf` | A performance improvement | patch |
| `docs` | Documentation only | none |
| `test` | Tests only | none |
| `refactor`, `style` | Code changes with no behavior change | none |
| `build`, `ci`, `chore` | Build, CI, tooling and housekeeping | none |

The scope is the component: `db-collector`, `config`, `chart`, `rules`,
`dashboards`, `kind-e2e`, `docsite`. Mark a breaking change with `!` after the
scope (`feat(config)!: …`) and explain it in a `BREAKING CHANGE:` footer.

## Before opening a pull request

Run the checks that cover what you changed; the
[test table](docs/guides/local-development.md#run-tests) lists each command
and what it needs. At minimum:

```bash
make test
make vet
```

Then:

- Update the docs in the same pull request. Each topic has one home (see
  [docs/README.md](docs/README.md)); link to it instead of repeating it.
- Tick the TODO items the pull request completes, and update the
  [roadmap](docs/product/roadmap.md) when a phase changes status.
- Record a decision that shapes the system as a new
  [ADR](docs/architecture/decisions/README.md). Working notes in
  `docs/plans/` and `docs/reviews/` are not tracked.
- Do not edit `CHANGELOG.md` by hand; it is generated at release.

## Pull requests

- Target `master`. Title: what the change does, with the TODO numbers in
  brackets, for example `db-collector: secure the collector endpoints [2.7]`.
- Description: what changed, why, and how it was tested.
- Required check: `ci-ok` (see [CI](#ci)).
- Merge with a merge commit; squash and rebase merging are disabled. The
  branch is deleted on merge.

With `git config fetch.prune true`, every fetch also removes the
`origin/*` copies of deleted branches; `git branch -vv` then shows merged local
branches as `gone`, ready for `git branch -d`.

## CI

`.github/workflows/ci.yml` runs on every pull request and every push to
`master`. The reasons are in
[ADR 0007](docs/architecture/decisions/0007-change-based-ci-with-one-required-check.md).

| Job | Runs |
| --- | --- |
| `Detect changes` | Always. Lists the paths the pull request changes; [`scripts/ci-changes.sh`](scripts/ci-changes.sh) decides which of the jobs below they need |
| `test` | Always: generated code is current, Go tests with the race detector, vet, Prometheus rules, docs site, PostgreSQL and Valkey integration tests |
| `SQL Server probes` | When `services/db-collector`, its test scripts or `tools/patientzero` (outside its chart) change; also runs PatientZero's live test |
| `Helm chart` | When the chart, its values, its tests, or PatientZero's chart or setup script change; lints, renders and checks PatientZero's chart too |
| `kind end-to-end` | When a service, the chart or the kind setup changes |
| `PatientZero scenarios` | On the release pull request only (release-please's branch, not its paths): every scenario on a fresh kind cluster, about 2.5 hours, through `.github/workflows/scenarios.yml` |
| `ci-ok` | Always. Passes only if every job did what the change detection asked for; the only required check |

Any path the script does not list (`go.mod`, `internal/`, `packages/config-schema`,
the Makefile, workflows, new directories) runs every job, and so does every
push to `master`. To force a full run on a pull request, run the `ci` workflow
by hand on its branch (Actions → ci → Run workflow).

Adding a job: give it `needs: changes` and an `if:` on a flag, add the flag to
`scripts/ci-changes.sh` and to `tests/ci_scripts_test.go`, and add the job to
`ci-ok`'s `needs` and arguments. The `scenarios` flag is the one exception:
it depends on the branch, so the `changes` job sets it itself.

`.github/workflows/scenarios.yml` runs every PatientZero scenario
([scenarios](tools/patientzero/scenarios/README.md)) nightly on `master`, by
hand (Actions → scenarios → Run workflow, any branch, every scenario or some)
and, through `ci.yml`, on the release pull request. Its artifact holds each
scenario's run journal and a summary. Run the same locally with
`make patientzero-scenarios` on a fresh cluster. Never add `paths:` to the workflow's
triggers: a required check whose workflow never starts is never reported, and
the pull request waits forever.

Runners are pinned too (`ubuntu-26.04`, not `ubuntu-latest`), so a new image
arrives as a reviewed change rather than on GitHub's schedule.

Actions and reusable workflows from other repositories are pinned to a full
commit SHA, with the exact version as a comment:
`uses: actions/checkout@<40-character SHA> # v7.0.1`. A tag can be moved to
other code; a commit cannot. `make test` fails on a `uses:` that is not pinned
this way (`tests/workflow_pins_test.go`), so the `test` job and `ci-ok` fail
too. To add or bump an action, resolve the tag to its commit with
`gh api repos/<owner>/<repo>/git/ref/tags/<tag>` (for an annotated tag, follow
`.object.sha` through `gh api repos/<owner>/<repo>/git/tags/<sha>`). Renovate
(`renovate.json`) proposes the updates, the SHA and the comment together, in
one weekly `GitHub Actions` pull request; majors wait for approval on the
[dependency dashboard](https://github.com/gdpais/Heartbeat/issues/86).

## Releases

A release is a `vX.Y.Z` tag on `master` with [Semantic Versioning](https://semver.org/);
one version covers both images and the chart. The rules and reasons are in
[ADR 0006](docs/architecture/decisions/0006-trunk-based-development-and-tagged-releases.md).

**Cutting a release.** Google's [release-please](https://github.com/googleapis/release-please)
(`.github/workflows/release-please.yml`) keeps a pull request named
`chore: release X.Y.Z` open on `master`. It updates `CHANGELOG.md`,
`version.txt` and the chart version from the Conventional Commits merged since
the last release; only `feat`, `fix` and breaking changes make one. Review the
entries, edit them in that pull request if needed, and merge it when you want
to release. Its CI runs every PatientZero scenario as well, about two and a
half hours, and `ci-ok` waits for them: a tag cannot be moved or deleted, so
a failing scenario must stop the release before it is tagged. release-please
then tags the merge commit and creates the GitHub
release, and the tag starts `.github/workflows/release.yml`, which:

1. checks the tag is on `master`, matches the chart version and passed the
   required CI check (`ci-ok`);
2. builds `ghcr.io/gdpais/heartbeat/db-collector` for `linux/amd64` and
   `linux/arm64`, and pushes the chart to `oci://ghcr.io/gdpais/charts/heartbeat`;
3. lists every artifact with its digest in the GitHub release.

While the version is `0.y.z`, every release is marked *Pre-release* on GitHub:
Heartbeat is a prototype until `1.0.0`.

A published version is never rebuilt. If a run fails after the tag exists, run
the `release` workflow by hand (Actions → release → Run workflow) with the tag;
it skips what is already published.

**Deploying.** Production runs a release by its image digests, promoted
through Argo CD ([ADR 0005](docs/architecture/decisions/0005-production-delivery-and-operations-defaults.md)).
There are no `latest` or `stable` tags.

**Tags** are created only by the release app and are never moved or deleted;
a bad release is fixed by the next patch release.

**One-time setup** ([TODO 0.5](TODO.md)), done at the end of phase 2. Until
the app exists the release-please job is skipped and no release is cut, not
even by hand: a hand release would have to bump the chart, `version.txt`,
`CHANGELOG.md` and the release-please manifest itself. Nothing is lost while
waiting; the first release pull request covers every commit since `v0.1.0`.

1. Create a GitHub App (Settings → Developer settings → GitHub Apps → New):
   webhook off; repository permissions *Contents*, *Pull requests* and
   *Issues*: read and write. Install it on this repository only.
2. In the repository's Settings → Secrets and variables → Actions, add the
   variable `RELEASE_APP_CLIENT_ID` (the app's client ID) and the secret
   `RELEASE_APP_PRIVATE_KEY` (a private key generated on the app's page).
3. Protect tags with two tag rulesets (Settings → Rules → Rulesets → New
   tag ruleset), both targeting all tags (*Include all tags*). Every tag here
   is a release tag, so all tags rather than `v*`: per-component tags such as
   `db-collector-v1.2.0` would not match `v*`.
   - `Heartbeat-tags`: *Restrict updates*, *Restrict deletions* and *Block
     force pushes*, with an empty bypass list, so no one can move or delete
     a tag.
   - `Heartbeat-tag-creation`: *Restrict creations* only, with the release
     app in the bypass list (*Always allow*). Add it after the first release
     is tagged by hand.

   A bypass applies to a whole ruleset, so the app gets a ruleset of its
   own: it may create tags but, like everyone else, not move or delete them.
   To check, pushing a tag by hand (`git push origin <tag>`) must be
   rejected.
4. After the first release, make each package public (your profile →
   Packages → package → Package settings → Change visibility).
