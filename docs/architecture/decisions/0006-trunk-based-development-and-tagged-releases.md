# 0006. Trunk-based development and tagged releases

- Status: Accepted. In effect from 2026-10-05, after phase 1 merged to
  `master`. Release workflows built; `v0.1.0` was tagged and published by
  hand on 2026-10-12, and the release app and tag-creation ruleset follow at
  the end of phase 2 ([TODO 0.5](../../../TODO.md)). The required
  checks are replaced by `ci-ok` ([ADR 0007](0007-change-based-ci-with-one-required-check.md)).
  Tag protection covers all tags, not only `v*`, so tags with other names
  cannot be moved either, and is split into two rulesets so the release
  app's bypass covers only tag creation.
- Date: 2026-10-05

## Context

Phase 1 (DB collector) was delivered on a long-lived phase branch,
`db-collectors`. Each TODO item had its own `feature/*` branch and pull request
into that branch, often worked by agents in parallel, and one pull request
merged the whole phase into `master` (#16).

That worked for one phase at a time but does not scale:

- Several phases running at once on separate phase branches drift apart. Shared
  files (`TODO.md`, the roadmap, the Helm chart, CI, contracts) conflict at the
  end, and work built on a stale `master` has to be redone.
- The phase branch fell behind `master` and had to be caught up by hand.
- Once `master` feeds production, merging a whole phase means one large deploy
  and a rollback that undoes the whole phase.

Nothing was versioned. The chart said `0.1.0`, images were tagged with a
content hash for kind, and production (ADR 0005) will run images pinned by
digest, but no rule said which commit becomes a release or where its images
live.

The repository builds two deployable images today, `db-collector` and
`otel-gateway` (later also the API, web UI and workers), plus one Helm chart
that deploys them together. Third-party engines (Prometheus, Grafana, Loki,
Alertmanager, OpenTelemetry Collector) come from their upstream registries,
pinned by the chart.

## Decision

| Concern | Choice | Why |
| --- | --- | --- |
| Branching | **Trunk-based development.** Short-lived `feature/*` branches from `master`, one per TODO item, merged back by pull request. No phase or integration branches | Work is integrated continuously, not at the end of a phase; parallel work across phases is limited only by real dependencies. |
| Merging | Merge commits only; head branches are deleted on merge; `master` requires a pull request and passing CI (`test`, `Helm chart`, `kind end-to-end`) | Merge commits keep each branch's commits intact, which the changelog reads. |
| Unfinished work | Merged but not reachable: not wired in yet, or behind a configuration flag. Schema changes follow expand, then contract | `master` stays releasable at every commit; ADR 0003 (risk 5) notes that a rollback does not reverse schema changes. |
| Versions | **One [Semantic Versioning](https://semver.org/) version for the repository.** A release versions every artifact together: both images and the chart are `0.2.0` in release `v0.2.0`, even if only one changed. `0.y.z` until the first production release | Atrium ships as one chart that deploys all its images, so one number answers "what is running". Per-component versions suit libraries released independently, and would need a version matrix between chart and images. |
| Releases | A `vX.Y.Z` tag on `master` creates a release. `release.yml` checks that the tag is on `master`, that the chart version matches, and that the required CI checks passed on that commit (waiting for them if they are still running), then builds and publishes | A release is an explicit point on `master` that passed the same checks as every pull request; tags themselves cannot require status checks. |
| Changelog | [`CHANGELOG.md`](../../../CHANGELOG.md), generated from [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) by Google's [release-please](https://github.com/googleapis/release-please), with [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) section names (Added, Changed, Fixed). release-please keeps an open release pull request with the next version, `CHANGELOG.md`, `version.txt` and the chart version; merging it creates the tag and the GitHub release | No hand-edited section for parallel branches to conflict on; tags come from a reviewed pull request, which fits a protected `master`. |
| Release identity | release-please runs with a **GitHub App** token (`RELEASE_APP_CLIENT_ID`, `RELEASE_APP_PRIVATE_KEY`) | Pull requests and tags created with the default `GITHUB_TOKEN` start no workflows: CI would never run on the release pull request, and the tag would not start the release. An app, unlike a personal token, is not tied to one person's account. |
| Artifacts | Multi-arch (`linux/amd64`, `linux/arm64`) images `atrium/<service>:<version>` with SBOM and provenance attestations, and the chart as an OCI artifact `charts/atrium`. A published version is never rebuilt or overwritten | A version names exactly one build; deployments pin the digest anyway (ADR 0005). |
| Registry | **GitHub Container Registry** (`ghcr.io/gdpais/...`) is the release registry. Production pulls from **Amazon ECR** (ADR 0005), which receives the same images when production delivery [18] is built | The repository and its artifacts are public; GHCR sits next to the code and the releases, needs no cloud account, and is pullable from kind, which makes the Argo CD rehearsal on kind [18] realistic. Copying an image keeps its digest, so both registries serve the identical build. |
| Pre-releases | Every `0.y.z` release is marked *Pre-release* on GitHub (release-please `prerelease`), so none is shown as *Latest*. The setting is removed for `1.0.0`, the first production release | Atrium is a prototype until then; releasing early still tests the release path and gives real artifacts for the Argo CD rehearsal. |
| Moving tags | None: no `latest` or `stable` Git tags or image tags. GitHub marks the newest release as *Latest* by itself | Moving tags make "what is deployed" ambiguous, which digest pinning (ADR 0005) exists to prevent. Promotion between environments is what says a release is stable. |
| Tag protection | Two tag rulesets on all tags: one forbids updates and deletions with no bypass; the other forbids creation, with the release app as its only bypass | A release cannot be moved or removed; a bad release is fixed by the next patch release. A bypass covers a whole ruleset, so a single ruleset would also let the app move and delete tags. |
| Promotion | Argo CD deploys from `atrium-deploy` (ADR 0005). Promoting a release is a commit there that sets the image digests for an environment: dev, then staging, then production | Every deploy is a reviewed commit that names an exact build; rollback is `git revert`. |
| Process | Contribution rules live in [`CONTRIBUTING.md`](../../../CONTRIBUTING.md) | One home for how to branch, commit, test, open pull requests and release, for people and agents. |

The first release, `v0.1.0`, is the phase 1 merge commit (`64ac1b6`): the
manifest starts at `0.1.0`, release-please counts commits after it, and the
tag is created once by hand with the release workflow run manually.

Considered and not chosen:

- **Phase branches for every phase.** Kept `master` at "last finished phase",
  but a finished phase is now marked by a release instead, without the drift.
- **GitFlow** (`develop`, `release/*`, `hotfix/*`). Built for shipping several
  supported versions; Atrium runs one deployment.
- **A hand-written changelog.** Every parallel branch would edit the same
  section and conflict.
- **Squash merging.** Rewrites each branch into one new commit, which breaks
  any long-lived branch that is merged again and hides the commits the
  changelog reads.
- **Publishing nothing until ECR exists.** Releases would carry no artifacts to
  rehearse with, and the release workflow would go untested until production.
- **Status checks on tags.** A tag has no pull request to check; the release
  workflow checks the tagged commit's CI instead.

## Consequences

- Every commit on `master` must pass CI and be safe to release. Large features
  land in several small pull requests, the unfinished parts switched off.
- Commit messages are release input: a `feat` commit bumps the minor version,
  `fix` the patch, and `!` or a `BREAKING CHANGE:` footer marks a breaking
  change, which bumps the major version (the minor while `0.y.z`, through
  `bump-minor-pre-major`). Other types (`docs`, `test`, `ci`, …) alone do not
  produce a release, and commits that are not Conventional Commits do not
  appear in the changelog.
- The chart's `version` and `appVersion` follow the release; the chart is no
  longer versioned on its own.
- The release app is a credential to look after: its private key is a
  repository secret, and the app is installed on this repository only.
- GHCR packages can start private when first published; each is made public
  once, in its package settings, so kind and Argo CD can pull without
  credentials.
- ECR publishing in production delivery [18] adds a registry to the same
  workflow (GitHub OIDC to AWS); it is not a second build pipeline.
- Documentation still publishes from `master` on every push, so docs can be
  ahead of the latest release.
- The `db-collectors` branch, its ruleset and its CI trigger are retired.
