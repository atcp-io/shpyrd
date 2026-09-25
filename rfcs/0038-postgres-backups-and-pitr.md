# RFC-0038 Postgres backups and point-in-time recovery

**Status:** implemented

**Owner:** unassigned

**Depends on:** RFC-0009 (implemented), RFC-0046

**Creation date:** 2026-09-22

**Last update:** 2026-09-25 (implemented and verified on OKE)

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

- The `postgres` extension installs the Barman Cloud CNPG-I plugin with the operator
  (component `barman-cloud`, the upstream manifest pinned); the platform's object store is
  RFC-0046's. A Postgres with `spec.backups` gets, in its namespace, an `ObjectBucket`
  (`<db>-backups`, so the key opens that database's archive and nothing else), the plugin's
  `ObjectStore` pointing at it, the cluster's `plugins` entry with `isWALArchiver` (PITR
  needs continuous WAL) and a `ScheduledBackup` (`immediate`, so the first base backup
  starts at once).
- `spec.backups: {schedule, retention}`: five-field cron in UTC (default `0 2 * * *`),
  `<n>d` (default `14d`, enforced by the plugin's `retentionPolicy`, which knows which WALs
  a base backup needs). `shpyrd pg create --backups [--retention 7d] [--backup-schedule
  "0 3 * * *"]`; `shpyrd pg backups enable|disable|list db`; `shpyrd pg backup db` takes
  one now and waits for it; `pg info` and the dashboard show "on, daily, kept 7d, last …,
  recoverable from …" (from the completed `Backup` objects: CloudNativePG fills its own
  summary fields late for plugin backups).
- Restore: `shpyrd pg restore db --as db-restored [--to 2026-09-25T16:58:02Z]` creates a
  Postgres with `spec.recovery: {from, targetTime}`, rendered as a cluster bootstrapped
  with `recovery` from the source's `ObjectStore` (`externalClusters` with the plugin and
  the source's `serverName`); the latest point when `--to` is omitted. Refused onto itself
  and before the earliest recoverable point. The restored database has its own `app`
  credential; `shpyrd attach` it when Ready.
- Switching backups off removes the schedule and the WAL archiver; the archive stays
  restorable until the database is deleted (the bucket goes with it).

## Design Details

- Controller (`internal/controller/postgres_backups.go`): bucket → store → schedule before
  the cluster, so archiving starts with the first instance; a bucket not yet ready reports
  Provisioning ("waiting for the backups bucket"), not Failed. The controller watches
  `Backup` objects to update the status when one completes, and requeues every five
  minutes while backups are on.
- Garage and barman-cloud (boto3) work together with the bucket's key, path-style
  addressing, region `garage`, gzip for WAL and data.
- Verified on OKE: create with backups, first base backup completed in a minute,
  continuous archiving healthy; rows written, on-demand backup, table dropped, restore to
  the second before the drop returned the rows in the new database.

## Implementation History

- 2026-09-22: RFC written.
- 2026-09-25: implemented on the object-storage extension (RFC-0046) and verified on OKE
  with a point-in-time restore. Released in v0.3.4.
