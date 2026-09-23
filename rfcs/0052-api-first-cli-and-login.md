# RFC-0052 API-first CLI and `shpyrd login`

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0031, RFC-0026

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

The CLI talks to the shpyrd API for project work instead of the Kubernetes API: `shpyrd
login` (OIDC device flow or an API token) gives it a user identity, so developers without
cluster access use every project command, actions are attributed to them, and roles apply.
Cluster operations keep the kubeconfig.

## Motivation

Today the CLI needs a kubeconfig with broad rights; the audit trail records `user@host`
rather than the person; roles do not apply to CLI users at all.

### Goals

- `shpyrd login` then `shpyrd deploy`, `logs`, `shell`, `run`, `scale`, config vars,
  resources, all through the API, with 403s explained by role.
- `shpyrd --cluster` (or a kubeconfig present) keeps the current direct mode for operators.

### Non-Goals

- Removing the direct mode.

## Proposal

- `shpyrd login [--url https://shpyrd.example.com]`: device flow against the issuer (Dex
  supports it) or `--token shp_...`; stored in `~/.shpyrd/config` per cluster URL.
- Project commands get an API transport: uploads, deploy, logs (NDJSON stream), shell and
  run (WebSocket exec from RFC-0026), scale/resize, config vars, volumes, attach/detach,
  members. Missing endpoints (`run` attach, `pg psql` via exec) are added.
- Mode selection: API when logged in, direct when a kubeconfig context is given
  explicitly; the same command surface either way.
- Audit entries from the API carry the user; `via: cli` stays for direct mode.

## Implementation History

- 2026-09-22: RFC written.
