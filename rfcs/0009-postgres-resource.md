# RFC-0009 Postgres resource

**Status:** implemented (create, attach, psql, HA instances); backups → RFC-0038, pooling/rotation/resize → RFC-0039

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

A `Postgres` project resource backed by the **CloudNativePG** operator (`postgres`
extension): create a database with a size and storage, attach it to an app and get
`DATABASE_URL`; backups to object storage, point-in-time recovery, connection pooling.

## Motivation

Almost every application needs a relational database; provisioning and wiring one is the
first thing people do manually after deploying.

### Goals

- `shpyrd pg create db` to `DATABASE_URL` in the app in one command.
- Production behaviour available when wanted: replicas, backups, PITR, pooling.
- Works on the local profile (MinIO for backups) and on cloud profiles (S3, RDS later).

### Non-Goals

- Multi-tenant shared database servers (one CNPG cluster per resource).
- Managed cloud databases (RDS) in the first version; a later `postgres-rds` extension can
  implement the same resource type.

## Proposal

| Operator | Notes |
| --- | --- |
| **CloudNativePG** (chosen) | Kubernetes-native, actively maintained, declarative `Cluster`, streaming replication and failover, backups to S3-compatible storage with PITR, PgBouncer `Pooler`, arm64 images |
| Zalando postgres-operator | mature, Patroni-based; heavier, older API style |
| Crunchy PGO | solid, larger footprint, commercial backing |
| StackGres | feature-rich, heavy |

```yaml
apiVersion: shpyrd.io/v1alpha1
kind: Postgres
metadata: { name: db, namespace: app-shop }
spec:
  version: "17"
  size: shared-m          # instance size from the catalog
  storage: 10Gi
  instances: 1            # 2-3 for HA
  backups:
    schedule: "0 3 * * *"
    retention: 14d
status:
  phase: Ready
  endpoint: db-rw.app-shop.svc:5432
```

The controller renders a CNPG `Cluster` (and optionally a `Pooler`); CNPG creates the app
user and its Secret; the `Binding` exposes `DATABASE_URL`, `PGHOST`, `PGPORT`, `PGUSER`,
`PGPASSWORD`, `PGDATABASE` (prefix configurable). Backups go to the object-storage
extension (MinIO locally, S3 on AWS).

CLI: `shpyrd pg create|list|info|psql|backup|restore|delete`. Dashboard: status,
endpoint, size, storage used, backups, "Attach".

## Design Details

- Extension component: CNPG operator chart (runlevel after cert-manager), plus the
  object-storage extension as a dependency for backups.
- Size mapping: instance size → CNPG `resources`; storage → PVC size (expandable).
- Credentials rotation: `shpyrd pg rotate db` regenerates the app password and produces a
  config release for bound apps.
- Deleting a Postgres resource requires typing its name and refuses while bound; the last
  backup is kept for the retention period.

### Drawbacks

- The operator is another component (~200 MB); enabled only when the extension is on.

## Implementation History

- 2026-09-22: RFC written; phase E.
- 2026-09-22: Implemented as the `postgres` extension: the CloudNativePG operator (chart
  0.29.0, runlevel rc2) as its component, a `Postgres` CRD (version, size, storage, instances)
  rendered into a CNPG `Cluster` (initdb bootstrap of database and owner `app`, resources
  from the size catalog raised to a 256 MiB memory floor because initdb OOMs below it,
  storage that grows but cannot shrink, superuser access off), status from the cluster's
  ready instances and the `<name>-app` Secret, endpoint `<name>-rw.<ns>.svc:5432`, a
  `Binder` exposing `DATABASE_URL|HOST|PORT|USER|PASSWORD|NAME`. `shpyrd pg
  create|list|info|psql|delete` (delete refused while attached), `shpyrd attach|detach`, the
  generic resources API and dashboard forms. The project network policy now admits every
  platform namespace so the operator can reach its instances. Not done: backups to object
  storage, PITR, `Pooler`, `shpyrd pg rotate`, RDS variant.
