# RFC-0025 Audit trail v2

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0008 (implemented), RFC-0022

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

A durable audit trail with long retention, a cluster-wide Audit page with filters, export,
and optional forwarding, replacing the one-hour Kubernetes Events of RFC-0008.

## Motivation

Compliance and incident review need "who did what" for months, across projects, and
exportable.

### Goals

- Every audited action (API and CLI) kept for the retention period and searchable by
  actor, project, action, time.
- Export as CSV/JSON; forward to a SIEM through a drain.

### Non-Goals

- Kubernetes API server audit logging (cluster-level; documented separately).

## Proposal

- Audit entries are written to the log pipeline as a dedicated stream (`stream=audit`,
  labels `project`, `actor`, `action`) in addition to the Event (kept for the project page's
  short view). Retention `SHPYRD_AUDIT_RETENTION` (default 365d) independent of app logs.
- `GET /api/audit?project=&actor=&action=&since=&until=` (platform roles: everything;
  project admins: their projects), dashboard Audit page (platform) and the project card
  reading from the durable store when available.
- Export: `shpyrd audit export --since 30d --format csv`.
- Forwarding: an audit drain reuses RFC-0023 sinks with `stream=audit`.

## Open questions

1. Retention default one year? Default: yes.
2. Is export enough, or must forwarding to a SIEM ship in the same RFC? Default: export
   here, forwarding through RFC-0023.

## Implementation History

- 2026-09-22: RFC written.
