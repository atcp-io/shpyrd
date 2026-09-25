# RFC-0037 Platform backup and restore

**Status:** implemented

**Owner:** unassigned

**Depends on:** RFC-0046

**Creation date:** 2026-09-22

**Last update:** 2026-09-25

## Summary

Scheduled, encrypted backups of the platform's state (every shpyrd object, config var
Secrets, memberships, sign-in users, install record, source archives) to an S3-compatible
bucket in the provider's object storage, outside the cluster, and
`shpyrd cluster restore --from s3://...` to bring that state back into a cluster that runs
the platform. Data of databases and volumes is covered by their own RFCs.

## Motivation

Losing the cluster today means recreating projects, config vars and members by hand.

### Goals

- Nightly backups without operator action, and one on demand.
- Backups unreadable without the passphrase.
- A restore that needs nothing but the archive, the passphrase and a cluster with the
  platform installed.

### Non-Goals

- Volume and database data (RFC-0038 for Postgres; volume snapshots are RFC-0060 and stay in
  the provider's snapshot store, not in the archive).

## Proposal

- **Component `platform-backup`** (every profile, rc4 next to `shpyrd`; skipped when no
  target is set): a CronJob in `shpyrd-system` running `shpyrd-server backup` from the server
  image under the server's ServiceAccount, on `SHPYRD_BACKUP_SCHEDULE` (default `0 3 * * *`
  UTC), keeping `SHPYRD_BACKUP_KEEP` archives (default 14). Hook `backup-target` generates
  the passphrase once (Secret `shpyrd-backup-key`) and stores the target in Secret
  `platform-backup-target`: `SHPYRD_BACKUP_TARGET` (`s3://bucket/prefix`),
  `SHPYRD_BACKUP_ENDPOINT`, `SHPYRD_BACKUP_REGION` and, when given, the access key.
- **Target**: the provider's object storage, so the archives outlive the cluster. Terraform
  roots of their own create the bucket apart from the cluster's state
  (`contrib/aws/terraform/backups`, `contrib/oci/terraform/backups`); the cluster root takes
  `backup_bucket` and grants access: on EKS a Pod Identity association for
  `shpyrd-system/shpyrd-server` (no keys), on OCI a Customer Secret Key of a dedicated IAM
  user written to `<name>-backups.env` for `--backup-credentials-file`. Both write the target
  into `<name>.vars`.
- **Archive**: a tar.gz with `manifest.json` (origin cluster, domain, profile, version,
  projects, counts), `system/` (install record, size catalog, global config vars, Dex
  `passwords` and `connectors`), `cluster/` (Teams, ProjectMembers), `projects/<ns>/`
  (Namespace, Secrets the platform owns — config vars first —, Apps, Volumes, Postgres, Redis,
  LogDrains) and `sources/<sha256>.tgz` (the blob sources apps build from, fetched from the
  server: on a fresh cluster the server's disk is gone). Objects are stripped of status,
  identity fields and owner references. The whole tarball is encrypted with age (scrypt
  passphrase), named `<domain>-<UTC timestamp>.tar.gz.age`.
- **CLI**: `shpyrd cluster backup` (a Job from the CronJob's template, waits, prints the
  archive), `shpyrd cluster backups` (target, schedule, last good, archives, recent runs),
  `shpyrd cluster backup key` (the passphrase, read from the cluster; keep it elsewhere),
  `shpyrd cluster restore --from s3://bucket/prefix[/archive] | --file <archive>
  --passphrase-file <file> [--credentials-file] [--endpoint] [--region] [--project <slug>]...
  [--no-system] [--overwrite] [--dry-run]`.
- **API**: `GET /api/cluster/backups` and `POST /api/cluster/backups` (cluster admin); the
  passphrase is never served. Dashboard: a "Platform backups" card on the cluster page with
  "Back up now".
- **Restore** runs against a cluster that already runs the platform (`shpyrd cluster init`
  with the new infrastructure's settings first: the archive's install record is for reading,
  the new record is authoritative). Order: system objects (created or replaced), then per
  project: Namespace → Secrets → sources uploaded to `POST /api/sources` → Volumes → Postgres
  → Redis → LogDrains → Apps; controllers build and start the apps. A project whose namespace
  exists is skipped unless `--overwrite`. `--project` restores a subset (the "deleted a
  project" case).

## Design Details

- Native Go: `pkg/backup` (Exporter, Restorer, S3 target via minio-go with static keys or the
  chain — environment, container credentials for Pod Identity, `~/.aws/credentials`),
  `filippo.io/age`. Velero considered for a later "with volume data" extension.
- The export reads with the server's ServiceAccount; the CronJob runs under it, which is
  also what the Pod Identity association on EKS targets.
- Secrets exported: those labelled `shpyrd.io/app` (config vars) or managed by shpyrd,
  minus what controllers regenerate (`shpyrd-registry`, `*-object-storage`, `*-tls`) and
  minus release snapshots (the release history does not travel).
- Manual runs are Jobs owned by the CronJob (garbage-collected with it; TTL 24h).

## Open questions

1. Native export (default) versus Velero? Resolved: native for state; Velero as an optional
   extension later.

## Implementation status

Implemented on 2026-09-25 and verified on OKE: nightly CronJob, `cluster backup`, listing,
key, `cluster restore --project` of a destroyed project (config var, volume, app rebuilt from
the archived source and serving again). Unit tests cover export, encryption round trip,
restore order, skip/overwrite semantics.

Still missing, on purpose or for later:

- **Database and volume contents.** Postgres archives (RFC-0038) live in the cluster's own
  object storage and die with the cluster: a database restored on a fresh cluster starts
  empty. Replicating the in-cluster store to the provider's bucket (RFC-0046 follow-up) closes
  this. Volumes are recreated empty; their snapshots stay in the provider but the objects
  that point at them do not travel.
- **Release history** is not restored: a restored app starts at v1 with its current config.
- **ApiTokens** are not exported: they do not exist yet (RFC-0052).
- **Restore does not install the profile**: `shpyrd cluster init` first; the RFC's original
  proposal had the restore do it, but the new infrastructure's settings (addresses, zone
  identifiers) are what `cluster init --vars-file` knows, not the archive.
- **No e2e in CI**; the loop above was run by hand.
- The `local` profile has no Terraform to create a bucket; `--backup-target` with any
  S3-compatible endpoint works (the in-cluster Garage too, which then defeats the purpose).
- A fresh OCI Customer Secret Key takes a few minutes to become usable
  (`SignatureDoesNotMatch` until then).

## Implementation History

- 2026-09-22: RFC written.
- 2026-09-25: implemented (component, CLI, API, dashboard card, Terraform roots).
