# How We Work

How Heartbeat is planned and delivered: what everyone agrees on, what the
team (people and agents) decides alone, how scope and reversals are handled,
and what the [board scorecard](#measures) watches. Branching, commits, checks
and releases are in [CONTRIBUTING.md](../../CONTRIBUTING.md); delivery order
and status are in [TODO.md](../../TODO.md).

## Outcomes over features

Each roadmap phase is a **user outcome**: who can now do what, and the
evidence that shows it. The outcomes are in the
[roadmap](roadmap.md#phase-outcomes). A phase is done when its outcome is
shown, not when its checklist is ticked.

- Every checklist item serves the current phase's outcome, or a later
  phase's. If it serves neither, it does not go in.
- Each phase ends with a **demo of the outcome** on real-shaped data
  ([test data](#test-data)): the user, the question they ask, and what
  Heartbeat answers. The demo is the phase's exit check, ahead of the release.
- Build a component in the same slice as its first caller. A part with no
  user and no caller waits.
- When two items compete, the one that moves the current outcome wins over
  hardening, tooling or docs for a later phase.

## Alignment and autonomy

Scope grows because needs are found while building. That is expected; the
answer is faster delivery, not a frozen plan. Delivery is fast when the team
can act alone inside clear limits, and the owner answers what only the owner
can answer, quickly.

**The owner sets** (alignment):

- the phase outcomes and their order ([roadmap](roadmap.md))
- the guardrails: [architecture decisions](../architecture/decisions/README.md),
  [CONTRIBUTING.md](../../CONTRIBUTING.md) and the agents' delivery policy
  (local to the owner's machine, not in git: what agents may commit, push and
  open without asking)
- what needs the owner: merging, releases, deploys, spending, credentials and
  access, compliance, and any change to an outcome or a guardrail

**The team decides alone** (autonomy), inside those limits:

- how to slice, build and test a phase's outcome
- adding, splitting or reordering items that serve the current outcome
- moving an item to a later phase, or to section 17, when it does not
- recording a decision as an ADR when it follows from the guardrails

**Owner actions are marked and answered fast.** An item only the owner can do
starts with **Owner** in TODO.md, with the date it became needed, or what it
waits for until then. The aim is
an answer within two working days. Status updates list open owner actions
first, oldest first. Everything else continues around them.

**Decisions come before the work that depends on them.** A platform,
compliance or organization question is answered, or explicitly deferred with
the cost of waiting written down, before code builds on one answer.

## Scope

New items are welcome; they are sorted when they arrive, not when they are
built:

| A new need… | Goes to |
| --- | --- |
| blocks the current phase's outcome, or is a defect | the current phase |
| serves a later phase's outcome | that phase's section |
| serves no planned outcome | section 17, until an outcome needs it |

Review findings follow the same rule. The scorecard shows items added against
items finished each week; the aim is to finish more than is added over a
month.

## Test data

Two kinds of data, for different questions:

| | PatientZero ([ADR 0010](../architecture/decisions/0010-patientzero-test-application.md)) | Replica of a real logging database |
| --- | --- | --- |
| Answers | Does Heartbeat produce the right signal when this fault happens? | Is what Heartbeat shows useful on a real application's data? |
| Data | Synthetic, shaped by the replica's survey; the answer is known | Real: one week of a real installation's screens, integrations, slow SQL and users |
| Live | Yes: load, faults, rotation, restarts | No: the one surveyed was a stale extract (`oslog_all`) with the native log tables empty, deleted after the survey |
| Use for | Fault scenarios, regressions, nightly CI | Today: its schema and traffic profile only ([ADR 0010](../architecture/decisions/0010-patientzero-test-application.md)). Outcome demos on its rows need a decision first |

A replica holds personal data ([ADR 0012](../architecture/decisions/0012-personal-data-in-telemetry.md)):
it is used only on the owner's machine and deleted once its numbers are
taken, and only DDL and aggregates reach git.

## Reversals

A **reversal** is merged work or an accepted decision that is later undone or
replaced: a component removed, a delivery path retired, an ADR superseded, a
configuration contract broken. Plans reordered before anything is built are
not reversals, and neither is ordinary fixing.

Reversals are part of learning; the aim is to find them sooner and cheaper.
Each one is logged here when its pull request merges, with its cause:

- **Late information**: a fact that existed but was not asked for in time
- **Built ahead of need**: no user or caller when it was built
- **Wrong assumption**: tested and found wrong
- **Changed need**: the need itself changed

| Reversed | What | Built or decided | Cause | Cost | Lesson |
| --- | --- | --- | --- | --- | --- |
| 2026-10-03 | Docker Compose platform and Kustomize bundle retired for one Helm chart on kind ([ADR 0003](../architecture/decisions/0003-helm-on-kind-and-production.md)) | 2026-06-04 | Late information | ~1,500 lines; a second delivery path kept for four months | Local, CI and production share one delivery path from the first slice |
| 2026-10-05 | Phase branch `db-collectors` retired for trunk-based development ([ADR 0006](../architecture/decisions/0006-trunk-based-development-and-tagged-releases.md)) | 2026-06-04 | Wrong assumption | A ruleset and a CI trigger | Short-lived branches on `master` by default |
| 2026-10-10 | Per-job required checks replaced by `ci-ok` ([ADR 0007](../architecture/decisions/0007-change-based-ci-with-one-required-check.md)) | 2026-10-05 | Wrong assumption | One ruleset change | Try a CI rule on a scenario pull request before requiring it |
| 2026-10-18 | OTel gateway service removed ([ADR 0009](../architecture/decisions/0009-remove-the-otel-gateway.md)) | 2026-06-13 | Built ahead of need | ~770 lines; planned as the next stage on 2026-10-04 | Build a component with its first caller |
| 2026-10-19 | Integrations config renamed to be platform-neutral; push endpoints dropped ([ADR 0011](../architecture/decisions/0011-platform-agnostic-telemetry-boundary.md)) | 2026-06-13 | Late information | A breaking config change | Ask which platforms the organization uses before naming one in a contract |
| 2026-11-02 | Traces sampled at 10% by default replaced by keeping every trace ([ADR 0018](../architecture/decisions/0018-heartbeats-own-telemetry.md)) | 2026-11-02 | Wrong assumption | One default, the ADR and its docs | Set a sampling or retention limit from a measured volume, not a convention |

The board scorecard counts reversals per month by cause.

## Measures

The board scorecard reads these from the repository and GitHub:

| Measure | Why |
| --- | --- |
| Phase outcomes shown | The product measure: what users can now do |
| Items finished and added per week | Delivery speed against growing scope |
| Open owner actions and their age | Where delivery waits on the owner |
| Reversals per month, by cause | How often merged work is undone |
| Time from a question to its decision | Late decisions are the main cause of reversals |
| Pull request time to merge, build pass rate, nightly scenario results | Flow and quality of each change |
