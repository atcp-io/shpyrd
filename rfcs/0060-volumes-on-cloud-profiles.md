# RFC-0060 Volumes on cloud profiles: storage classes, provider minimums, snapshots

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0006 (persistent volumes, implemented), RFC-0035 (cloud profiles);
RFC-0041 (shared volumes) for the shared class

**Creation date:** 2026-09-24

**Last update:** 2026-09-24

## Summary

RFC-0006 gives projects volumes on whatever storage class the cluster defaults to, which is
right on kind and mostly right on a cloud, where the provider's block storage attaches disks
that outlive nodes. This RFC makes volumes a first-class part of a cloud profile: the
profile names the classes for single-instance and shared volumes and everything else on
the platform (registry, Postgres, Redis) uses them; provider minimums and binding rules are
explained instead of surprising people; volumes can be snapshotted and restored; and
detached disks that still cost money are visible.

## Motivation

On OKE a `shpyrd volumes create data --size 1Gi` silently produces a 50Gi disk (Oracle's
minimum), the claim stays `Pending` until a process mounts it (`WaitForFirstConsumer`) and
looks stuck, a project destroyed with `Retain` leaves a disk that keeps billing, and there
is no way to take a backup before a risky release. Every provider has a variant of each.

### Goals

- A profile declares its storage classes once; nothing else on the platform hard-codes one.
- Sizes and states shown to users are the effective ones, with the provider rule that
  produced them.
- `shpyrd volumes snapshot|restore` where the provider supports CSI snapshots.
- Detached disks are listed and can be purged.

### Non-Goals

- Provisioning storage services (mount targets, file systems) outside Kubernetes; the
  profile's infrastructure scripts create them and the profile references them.
- Cross-region replication and disaster recovery (RFC-0037).
- Database backups (RFC-0038 uses object storage, not disk snapshots).

## Proposal

- Profile vars `SHPYRD_STORAGE_CLASS` (single-instance, RWO) and `SHPYRD_STORAGE_CLASS_SHARED`
  (RWX), `SHPYRD_VOLUME_MIN_SIZE`, `SHPYRD_SNAPSHOT_CLASS`. Local: `standard`, the
  `storage-rwx` extension's class, no minimum, no snapshots. `oci`: `oci-bv` (balanced
  performance, expansion allowed), `shpyrd-fss` (OCI File Storage, created by the profile
  from `SHPYRD_FSS_MOUNT_TARGET_SUBNET`), 50Gi, `oci-bv-backup` (incremental block volume
  backups). AWS (RFC-0035): `gp3`, `efs`, 1Gi, `ebs-snapshot`.
- The Volume controller, Postgres, Redis and the registry claim on the profile's classes
  when no class is given; `--class` stays for the exceptions (a higher performance tier).
- Minimums: the CLI, API and dashboard round a request up to `SHPYRD_VOLUME_MIN_SIZE` and
  say so ("Oracle Cloud block volumes start at 50Gi; created data at 50Gi"), before the
  disk exists. The size shown afterwards is the claim's capacity, as today.
- Binding: a volume whose class binds on first consumer shows "ready to mount" instead of
  Pending, with the hint that the disk is created when a process mounts it.
- Snapshots: `shpyrd volumes snapshot data [--name pre-release]`, `shpyrd volumes snapshots
  data`, `shpyrd volumes restore data --from pre-release [--to data-copy]` (restore creates a
  new volume; in-place restore stops the mounting process, swaps the claim and starts it,
  as one release-free operation with confirmation). The dashboard's Volumes card grows a
  Snapshots list with create and restore.
- Detached disks: `shpyrd volumes detached` lists PersistentVolumes in `Released` state
  from this platform (project, name, size, age, monthly cost estimate when RFC-0048 exists);
  `shpyrd volumes purge <pv>` deletes one after confirmation; the cluster page counts them.
- Docs: a "Volumes on Oracle Cloud" section (minimums, performance tiers, in-transit
  encryption as a node pool setting, File Storage for shared volumes, costs).

### Alternatives

- **Let the CSI driver round up silently** (status quo). Users see a 1Gi volume become
  50Gi after the fact and a surprise on the bill.
- **`Delete` reclaim policy on cloud** so nothing is left behind. Loses data on an
  accidental `projects destroy`; RFC-0006 chose `Retain` deliberately. Visibility plus an
  explicit purge keeps both.
- **Backups through object storage instead of CSI snapshots.** Right for databases
  (RFC-0038) and the platform (RFC-0037); for arbitrary files on a disk the provider's
  snapshot is the fast, consistent tool.

## Design Details

- `Volume` CRD gains `status.effectiveSize`, `status.binding` (`immediate` |
  `onFirstMount`) and `status.snapshots[]` (name, created, size, ready); the App API's
  volume responses carry them.
- Snapshot support is detected from the `snapshot.storage.k8s.io` CRDs and a class named by
  `SHPYRD_SNAPSHOT_CLASS`; the `oci` profile installs the external-snapshotter CRDs and
  controller as a component (`snapshot-controller`) and creates the class
  (`driver: blockvolume.csi.oraclecloud.com`, `backupType: incremental`, `deletionPolicy:
  Delete`). Without support, the commands explain what is missing.
- Restore in place: the controller scales the mounting process to zero (RFC-0006 processes
  with RWO volumes are single-instance already), creates a claim from the snapshot with the
  same size, rebinds the Volume, keeps the old claim as `Released` for a purge later,
  scales back up. Recorded in the activity feed.
- Shared volumes on OKE: a StorageClass `shpyrd-fss` with `provisioner:
  fss.csi.oraclecloud.com`, `availabilityDomain`, `mountTargetSubnetOcid` and
  `encryptInTransit: "true"`; File Storage ignores the size (billed by use) and the
  dashboard says so.
- Cost hint: with RFC-0048 absent, a static table per profile (OCI block volume balanced
  price per GB-month, FSS per GB-month) gives the estimate on the detached list only.

## Open questions

1. Round up to the provider minimum with a message, or refuse sizes below it? Default:
   round up and say so.
2. In-place restore as part of the first version? Default: yes, with confirmation; restore
   to a new volume is the safe path the dashboard offers first.
3. Install the snapshot controller on `oci` by default? Default: yes (OKE ships the CSI
   snapshotter but not the CRDs or the controller).

## Implementation History

- 2026-09-24: RFC written after the OKE proof of concept.
