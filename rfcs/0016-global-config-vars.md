# RFC-0016 Global config vars

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0003 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

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

## Open questions

1. Opt-out per project and per-key exclusion as above? Default: yes, both.
2. Team-scoped globals (a team's projects only)? Default: not now; the model allows a
   second Secret per team later.

## Implementation History

- 2026-09-22: RFC written.
