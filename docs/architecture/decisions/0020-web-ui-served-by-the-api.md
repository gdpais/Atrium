# 0020. Web UI: a Vite single-page app served by the API

- Status: Accepted
- Date: 2026-11-04

## Context

Phase 4's outcome is that an operator onboards an application, a SQL Server
target and its probes in a browser, with every change audited
([roadmap](../../product/roadmap.md)). The API already does all of it
[TODO 4.1–4.5].

What is fixed already:

- TypeScript and React, no server-side rendering
  ([ADR 0004](0004-core-technology-choices.md)).
- The UI owns nothing durable and talks only to the API, from the same origin
  ([ADR 0013](0013-api-design-and-subsystem-boundaries.md) §1). The session
  is a `__Host-` cookie (`Secure`, `SameSite=Lax`, `Path=/`), and every unsafe
  request needs an `Origin` equal to `HEARTBEAT_API_SITE_ORIGIN` (§8).
- One chart for kind, CI and production, rendered by Argo CD
  ([ADR 0003](0003-helm-on-kind-and-production.md),
  [ADR 0005](0005-production-delivery-and-operations-defaults.md)); one version
  for every image ([ADR 0006](0006-trunk-based-development-and-tagged-releases.md));
  CI jobs run when a change can affect them, behind one required check
  ([ADR 0007](0007-change-based-ci-with-one-required-check.md)).
- The UI's client is generated from `apps/api/openapi.yaml` (ADR 0013 §4).

Open: how the UI is built, where it is served from, which libraries it uses
and how it is tested.

## Decision

1. **The API serves the UI.** The built app is embedded in the API binary
   (`embed.FS`) and served at `/`. `/api/*`, `/metrics` and the health
   endpoints are unchanged; any other `GET` that accepts HTML gets
   `index.html`, so client routes survive a reload. Hashed assets are cached
   for a year as immutable, `index.html` is `no-cache`, and every UI response
   carries a strict Content-Security-Policy: scripts from the origin only,
   never inline (styles may be inline, since Mantine sets CSS variables at
   run time). One image
   and one origin by construction: no new Deployment, Service,
   NetworkPolicy or ingress rule, and kind keeps working through the existing
   port-forward.
2. **Build with Vite, TypeScript strict, in `apps/web`.** Node is pinned to
   the active LTS in `apps/web/.nvmrc` and checked by `make tools-check`;
   packages come from npm with a committed lockfile (`npm ci`), at exact
   versions, kept current by Renovate.
3. **Typed client generated from `openapi.yaml`** with `openapi-typescript`
   (types only) and `openapi-fetch` (a small typed `fetch`). `make generate`
   writes it and `make generate-check` fails when it is stale, as it does for
   oapi-codegen and sqlc.
4. **TanStack Query for server state, React Router for routes**, no global
   store. The UI reads the user's roles from `GET /api/v1/session` to show or
   hide actions; the API stays the authority, and the UI handles a 401 or 403
   rather than trying to prevent it.
5. **Mantine for components** (forms, tables, modals, notifications), with a
   light theme of our own and no Tailwind toolchain.
6. **Tests.** A `web` CI job, run when `apps/web` or `openapi.yaml` changes and
   counted by `ci-ok`, runs ESLint, `tsc --noEmit`, Vitest with Testing
   Library, the build and `npm audit`. Playwright (Chromium only) drives the
   UI against the real API in `kind-e2e`: log in, onboard PatientZero, approve
   a probe assignment, follow a deep link.
7. **Development loop.** On kind the port-forwarded API serves the built UI
   at `http://localhost:8081`, which is already kind's `api.siteOrigin`. For
   hot reload, `npm run dev` serves the app with Vite and proxies `/api` to
   that port-forward, sending `Origin: http://localhost:8081` so the API's
   check passes unchanged; cookies are not scoped by port, so the session
   works on both. Chromium and Firefox treat `localhost` as a secure context,
   so the `__Host-` cookie works over plain HTTP there.
8. **The API image builds the UI.** A Node stage on the build platform runs
   `npm ci` and `npm run build`; the Go stage embeds `apps/web/dist`. A Go
   build without a bundle embeds a placeholder page, so `go test` and
   `go build` never need Node.

## Consequences

- The UI and the API ship together, at one version, in one image; a UI-only
  change rebuilds the API image. That fits ADR 0006 and costs about 1 MB of
  assets in the image.
- The API grows a small static handler, its cache and CSP headers and the
  `index.html` fallback, all tested in Go.
- The UI is not a pod, so the API's NetworkPolicy gains no peer for it and
  the kind memory budget [TODO 0.4] is unchanged.
- npm brings a dependency tree into the repository: exact versions, a
  lockfile, `npm audit` in CI and Renovate keep it reviewable.
- `kind-e2e` gains a browser run; it must stay within the job's 25-minute
  limit, measured when Playwright is added.
- Investigation, alert and report screens are built in phase 7 with their
  APIs and workers, not as shells now.

## Alternatives considered

- **A separate static container (nginx or distroless) behind an ingress that
  splits `/` and `/api`.** A second pod, image and release artefact, and kind
  would need an ingress just to get one origin. Worth revisiting only if the UI
  and the API must release on different schedules.
- **Next.js or another SSR framework.** ADR 0004 rules out SSR, and an
  operator console needs none.
- **pnpm.** Faster installs, but one more tool to pin; npm ships with Node.
- **orval or openapi-generator for the client.** More generated code to review
  than types plus a 6 kB `fetch` wrapper.
- **Radix primitives with our own CSS, or MUI.** Radix gives more control but
  is slower to the first screen; MUI is heavier. Mantine covers the forms and
  tables the onboarding screens need.
