# RFC-0011 Project identity and product language

**Status:** provisional

**Owner:** unassigned

**Depends on:** none

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

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
- **URLs**: dashboard routes `/projects/<slug>` (tabs as query or subpaths), `/apps/:ns/:name`
  redirects for one release. API: new routes `/api/projects/<slug>/...` mirroring today's
  `/api/apps/:ns/:name/...` (the server derives the namespace); old routes stay one release
  with a `Deprecation` header. Project-level routes already use `/api/projects/:ns/...`
  with the namespace; they move to the slug too.
- **Copy audit**: replace "pod" with "instance" in the dashboard (metrics tooltips use
  instance names via the existing `InstanceNames` mapping; build steps say "build step",
  "build instance"), CLI messages, docs. `kubectl` names stay in `kubectl` output only.

## Design Details

- `pkg/api`: a `projectOf(slug)` helper resolving namespace `app-<slug>`; route table
  registered twice during the deprecation window.
- UI: router paths, `Link` targets, breadcrumb; a `displayName(app)` helper.
- CLI: `projects create` accepts a free-form name, prints "Created project My Shop (my-shop)";
  `--slug` overrides.

## Open questions

1. Keep the `App` kind as the internal name? Default: yes.
2. Keep old dashboard/API paths for one release with redirects and a `Deprecation`
   header, or break now? Default: keep for one release.

## Implementation History

- 2026-09-22: RFC written.
