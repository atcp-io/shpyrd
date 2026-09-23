# RFC-0016 Global config vars

**Status:** implemented

**Owner:** Patrick Negri (shpyrd-io/shpyrd main)

**Depends on:** RFC-0003 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-23

## Summary

Cluster-wide config vars (an `OPENAI_API_KEY` every project should have) set once by a
platform admin and injected into every process of every project, write-only like project
config vars, overridable per project, and part of the release fingerprint.

## Motivation

Shared credentials and settings are copied into each project by hand today and drift.

### Goals

- `shpyrd globals set OPENAI_API_KEY=...` and every app has it after its next release.
- Project values win over global ones; attached resources win over both.
- Values never shown; changes audited and released.

### Non-Goals

- Per-team globals or environment-specific sets (see questions).

## Proposal

- Secret `shpyrd-system/shpyrd-global-env` managed by `shpyrd globals set|unset|list` and a
  Cluster page card (platform admins, `cluster.admin`).
- The App controller mirrors it into each project namespace as `shpyrd-global-env` (owned
  by nothing, refreshed on change; deleted with the namespace) and adds it to `envFrom`
  **first**: order global < `<app>-env` < `<app>-bindings`, so later sources win.
- The global Secret's content joins `configHash`; a change produces a release "Global
  config change" in every app (the Cluster card says how many projects will restart and
  asks for confirmation).
- `shpyrd secrets list` and the Config tab show global vars read-only with "provided by
  cluster"; a project setting the same name shadows it (shown as "overrides global").
- Opt-out per project: `shpyrd.yaml` `globals: false` or `globals: {exclude: [NAME]}`.

## Design Details

- Watch: the App controller already watches Secrets; add a map from the global Secret to
  every App (like the sizes ConfigMap).
- `configvars` metadata (updatedAt per key) reused for the global Secret.
- Audit: `globals.set`, `globals.unset` with names only.

## Settled questions

1. Opt-out per project and per-key exclusion as above? Yes, both: `App.spec.globals`
   `{disabled, exclude}`, written by `shpyrd.yaml`'s `globals: false | {exclude: [...]}`.
2. Team-scoped globals (a team's projects only)? Not now; the model allows a second
   Secret per team later.

## Implementation History

- 2026-09-22: RFC written.
- 2026-09-23: implemented in shpyrd-io/shpyrd. Notes:
  - The mirror in each project namespace is filtered by the project's opt-out (excluded
    keys never reach the namespace) and carries the per-key `updatedAt` metadata of the
    keys it holds; it is owned by nobody and removed when the project receives nothing.
  - `Release.globalHash` fingerprints the globals a project received; a release whose
    project vars did not change but whose global hash did is described "Global config
    change". Globals join `configHash` under their own prefix, so the same key moving
    between a binding and a global is a change. Rollbacks restore project vars only:
    the globals in effect are always the current ones.
  - One-off commands (`shpyrd run`, `shpyrd shell`) read the same three sources in the
    same order as deployed processes (`controller.EnvSources`).
  - API: `GET/PUT /api/globals` (`cluster.admin`) return names, `updatedAt` and the
    number of projects that receive globals; `GET /api/projects/<slug>/secrets` gained
    `global` (the project's mirror). Audit: `globals.set` / `globals.unset`, names only.
  - CLI: `shpyrd globals set|unset|list`; `shpyrd secrets list` prints `cluster` in
    PROVIDED BY and "(overrides global)" on shadowing project vars.
  - Dashboard: a Global config vars card on the Cluster page (platform admins) with a
    confirmation naming the number of projects that will release; the Config tab shows
    cluster-provided rows and an "overrides global" marker.
