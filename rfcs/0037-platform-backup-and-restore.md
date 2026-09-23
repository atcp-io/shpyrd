# RFC-0037 Platform backup and restore

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0046

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Scheduled, encrypted backups of the platform's state (every shpyrd object, config var
Secrets and snapshots, memberships, users, install record) to S3-compatible storage, and
`shpyrd cluster restore --from s3://...` to rebuild the control plane on a fresh cluster.
Data of databases and volumes is covered by their own RFCs.

## Motivation

Losing the cluster today means recreating projects, config vars and members by hand.

### Goals

- Nightly backups without operator action; restore tested by an e2e.
- Backups unreadable without the passphrase.

### Non-Goals

- Volume and database data (RFC-0038 for Postgres; volume snapshots later).

## Proposal

- `shpyrd cluster backup [--to s3://bucket/prefix]` and a CronJob in `shpyrd-system`
  exporting: Apps, Volumes, Postgres, Redis, Teams, ProjectMembers, ApiTokens, Dex Password
  objects, Secrets labelled `shpyrd.io/managed-by` (config vars, release snapshots,
  credentials), the install record; serialised as a tarball encrypted with `age` (passphrase
  in Secret `shpyrd-backup-key`, printed once at setup) and uploaded to the object store.
- `shpyrd cluster restore --from ... --passphrase-file ...`: installs the recorded profile
  and extensions, recreates namespaces and objects in dependency order, then lets
  controllers rebuild workloads.
- `shpyrd cluster backups list`, retention count.

## Design Details

- Native implementation (Go, `age`, S3 SDK); Velero considered for a later "with volume
  data" extension.

## Open questions

1. Native export (default) versus Velero? Default: native for state; Velero as an optional
   extension later.

## Implementation History

- 2026-09-22: RFC written.
