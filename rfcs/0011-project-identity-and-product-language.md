# RFC-0011 Project identity and product language

**Status:** implemented (with gaps) — see Implementation status below

**Owner:** Patrick Negri (shpyrd-io/shpyrd main)

**Depends on:** none

**Creation date:** 2026-09-22

**Last update:** 2026-09-23

## Summary

Projects get a display name and a URL-safe slug, dashboard and API paths say `projects`
instead of `apps`, and nothing user-facing says "pod": instances are named `web.1`,
`worker.2` everywhere, including metrics tooltips and build steps.

## Motivation

`shpyrd projects create "My Shop"` fails today (names must be DNS labels), the dashboard
lives at `/apps/app-hello-docker/hello-docker` while the product speaks of projects, and
Kubernetes vocabulary leaks into copy ("build pod", pod names in chart tooltips). These
are the first things a new user notices.

### Goals

- Any human name works; the slug is derived and shown next to it.
- One URL scheme: `/projects/<slug>` in the dashboard, `/api/projects/<slug>/...` in the API.
- No "pod" in the dashboard, CLI output or docs; "instance" and `web.1` names instead.

### Non-Goals

- Renaming the `App` CRD or the namespace scheme (`app-<slug>`): internal, unchanged.

## Proposal

- **Display name**: annotation `shpyrd.io/display-name` on the App; `shpyrd projects
  create "My Shop"` derives slug `my-shop` (lowercase, dashes, max 40, trailing dashes
  trimmed, conflict → error suggesting `my-shop-2`). The dashboard's New project form
  shows the slug live under the name field. Lists show the display name with the slug in
  small type; the slug stays the CLI identifier and the hostname.
- **URLs**: dashboard route `/projects/<slug>` (tabs as query). API: `/api/projects` and
  `/api/projects/<slug>/...` replace `/api/apps/:ns/:name/...`; project-level routes that
  took the namespace (`/api/projects/:ns/volumes`...) take the slug. The server derives the
  namespace; a path whose slug is malformed is a 404. The old routes are gone: pre-alpha,
  no redirects.
- **API shape**: list and detail carry `slug` and `displayName` (no `name` field, so that
  clients cannot confuse the two); `POST /api/projects {name, slug?}` takes the display
  name and an optional slug; `PATCH /api/projects/<slug> {name}` renames. Every project
  action is audited with "Display Name (slug)" as target.
- **Copy audit**: replace "pod" with "instance" in the dashboard (metrics tooltips use
  instance names via the existing `InstanceNames` mapping; the log viewer no longer shows
  the pod name on hover; build steps say "build failed before step"), CLI and API
  messages, docs and sample apps. `kubectl` names stay in `kubectl` output only.

## Design Details

- `pkg/project`: the one place with the naming rules: `Slug(name)` (NFD, accents dropped,
  lowercase ASCII, single dashes, 40 characters), `ValidSlug`, `Namespace`, `FromNamespace`,
  `DisplayName`/`SetDisplayName`/`Label`. `authz.ProjectFromNamespace` and
  `resources.Namespace` delegate to it.
- `pkg/api`: `projectKey(c)`/`projectNamespace(c)` read `:slug`; `require()` rejects
  malformed slugs before authorization.
- UI: `api.ts` takes the slug only; `slugify()` mirrors `pkg/project.Slug` so the New
  project form shows the slug live (editable; it follows the name until touched); the
  header shows the display name with the slug beside it and a rename dialog; destroying
  asks for the slug.
- CLI: `projects create "My Shop"` derives the slug, `--slug` overrides, prints "Created
  project My Shop (my-shop)"; `projects rename <slug> "<name>"`; `list` shows PROJECT
  (slug) and NAME; `--project` and `shpyrd.yaml` keep taking the slug.

## Open questions

1. Keep the `App` kind as the internal name? Settled: yes.
2. Keep old dashboard/API paths for one release with redirects and a `Deprecation`
   header, or break now? Settled: break now (pre-alpha, no external users).

## Implementation History

- 2026-09-22: RFC written.
- 2026-09-23: open questions settled (break now); implemented in shpyrd-io/shpyrd (API, CLI, dashboard, copy audit).

## Implementation status

Audited on 2026-09-25 against the code. What the text promises but the platform does not do yet is listed here; superseded means a later RFC decided otherwise and the text above is history.

- **Not implemented:** The logs-agent extension description still says "pods" (`shpyrd extensions list`, cluster page).
