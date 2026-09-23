# RFC-0038 Postgres backups and point-in-time recovery

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0009 (implemented), RFC-0046

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Scheduled and on-demand backups of Postgres resources to object storage through
CloudNativePG's Barman Cloud plugin, retention, and restore to a point in time as a new
database.

## Motivation

A database without backups is not production; it is the first question users ask.

### Goals

- `spec.backups: {schedule, retention}` and `shpyrd pg backup db`; last backup visible.
- `shpyrd pg restore db --to "2026-09-22T10:00Z" --as db-restored`.

### Non-Goals

- In-place restore (a restored database is a new resource the app is re-attached to).

## Proposal

- The `postgres` extension installs the Barman Cloud CNPG-I plugin with the operator;
  the controller renders an `ObjectStore` per project (bucket path `postgres/<project>/`,
  credentials from RFC-0046) and, for a Postgres with `spec.backups`, a `ScheduledBackup`
  plus the cluster's `plugins` entry for WAL archiving (PITR needs continuous WAL).
- `shpyrd pg backup db` creates a `Backup`; `shpyrd pg backups db` lists them with size and
  status; `pg info` shows the last backup and the recovery window.
- Restore: a new Postgres with `spec.recovery: {from: db, targetTime}` renders a cluster
  bootstrapped with `recovery` from the object store; when Ready, `shpyrd attach` it.
- Retention `spec.backups.retention` (e.g. `14d`) enforced by the plugin.

## Design Details

- Object store credentials per project namespace as a Secret copied by the controller.
- e2e: create, write rows, backup, drop table, restore to before the drop, assert rows.

## Implementation History

- 2026-09-22: RFC written.
