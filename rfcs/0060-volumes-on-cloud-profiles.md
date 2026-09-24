# RFC-0060 Volumes on cloud profiles: storage classes, provider minimums, snapshots

**Status:** implemented (shared volumes on OKE: built, awaiting the tenancy's File Storage limit to verify end to end)

**Owner:** unassigned

**Depends on:** RFC-0006 (persistent volumes, implemented), RFC-0035 (cloud profiles);
RFC-0041 (shared volumes) for the shared class

**Creation date:** 2026-09-24

**Last update:** 2026-09-25

## Summary

RFC-0006 gives projects volumes on whatever storage class the cluster defaults to, which is
right on kind and mostly right on a cloud, where the provider's block storage attaches disks
that outlive nodes. This RFC makes volumes a first-class part of a cloud profile: the
profile names the classes for single-instance and shared volumes and everything else on
the platform (Postgres, Redis) uses them; provider minimums and binding rules are
explained instead of surprising people; and volumes can be snapshotted and restored.

## Motivation

On OKE a `shpyrd volumes create data --size 1Gi` silently produces a 50Gi disk (Oracle's
minimum), the claim stays `Pending` until a process mounts it (`WaitForFirstConsumer`) and
looks stuck, a freshly formatted disk belongs to root while app processes never run as root
(nothing could write to it), shared volumes have no provisioner at all, and there is no way
to take a backup before a risky release. Every provider has a variant of each.

### Goals

- A profile declares its storage classes once; nothing else on the platform hard-codes one.
- Sizes and states shown to users are the effective ones, with the provider rule that
  produced them.
- `shpyrd volumes snapshot|restore` where the provider supports CSI snapshots.
- Any image user can write to a mounted volume.

### Non-Goals

- Provisioning storage services (mount targets, file systems) outside Kubernetes; the
  profile's infrastructure scripts create them and the profile references them.
- Cross-region replication and disaster recovery (RFC-0037).
- Database backups (RFC-0038 uses object storage, not disk snapshots).

## Proposal

- Profile vars `SHPYRD_STORAGE_CLASS` (single-instance, RWO), `SHPYRD_STORAGE_CLASS_SHARED`
  (RWX), `SHPYRD_VOLUME_MIN_SIZE`, `SHPYRD_SNAPSHOT_CLASS`. Local: the cluster default, no
  shared class until RFC-0041, no minimum, no snapshots. `oci`: `oci-bv` (balanced
  performance, expansion allowed), `shpyrd-fss` (OCI File Storage), `50Gi`, `oci-bv-backup`
  (incremental block volume backups). AWS (RFC-0035): `gp3`, `efs`, `1Gi`, `ebs-snapshot`.
- The Volume controller, Postgres and Redis claim on the profile's classes when no class is
  given; `--class` stays for the exceptions (a higher performance tier). The registry claim
  keeps the cluster default (it is created before the platform knows anything).
- Minimums: the API rounds a request up to `SHPYRD_VOLUME_MIN_SIZE` and says so in the
  response (`note`), which the CLI prints and the dashboard toasts: "Oracle Cloud block
  volumes start at 50Gi: created at 50Gi instead of 1Gi"; the dashboard's size field also
  says it up front. Postgres and Redis apply the same rule in their controllers and report
  the effective size in `status.storage`, shown as "50Gi (5Gi requested; provider minimum)".
  Shared volumes are file systems and skip the rule (File Storage ignores the size and bills
  by use; the response says so).
- Binding: a volume whose class binds on first consumer shows "created; the disk is
  provisioned when a process mounts it" instead of a bare Pending.
- Ownership: pods mounting volumes get `fsGroup: 1000` with `fsGroupChangePolicy:
  OnRootMismatch`, so the kubelet hands a fresh disk to a group every container is in and
  any image user can write; the recursive chown happens once, not on every start. Shared
  (NFS) volumes are covered by their export options instead (below).
- Snapshots: `shpyrd volumes snapshot data [--name pre-release]`, `shpyrd volumes snapshots
  data` (also `snapshot list|rm`), `shpyrd volumes restore data --from pre-release [--to
  data-copy] [--yes]`. Restore into a new volume creates a Volume with `spec.fromSnapshot`
  (the claim's `dataSource`), at least the snapshot's size. Restore in place is a request
  on the Volume (`shpyrd.io/restore-from` plus a `shpyrd.io/restore-id`); the controller
  turns the Volume `Restoring`, the App controller scales the processes mounting it to zero
  ("stopped while volume data is restored"), the old claim is deleted once no pod uses it,
  a new claim is created from the snapshot and stamped with the request id, and the request
  is cleared so the processes come back (on OKE the disk binds when they mount it). The
  request id keeps a second restore from the same snapshot from being mistaken for done.
  `status.restoredFrom` names the snapshot afterwards. Neither path creates a release; both
  are in the activity feed. The dashboard's Resources card gets a Snapshots dialog per
  volume (take, restore into a new volume first, in place, delete).
- Shared volumes on OKE: `contrib/oci/terraform` (`shared_storage = true`) creates one File
  Storage mount target in the workers subnet behind its own security group (NFS from the
  workers only), plus the IAM policy that lets the cluster's CSI plugin create file
  systems (`request.principal.type = 'cluster'`, scoped to the cluster id). `shpyrd cluster
  init --set SHPYRD_FSS_MOUNT_TARGET=<ocid> --set SHPYRD_FSS_AD=<ad>` installs the
  `storage-fss` component: StorageClass `shpyrd-fss` (`fss.csi.oraclecloud.com`,
  `mountTargetOcid`, export options squashing every client to root). Without the mount
  target the component is skipped and a shared volume is refused at creation with the
  instructions; the Volume controller refuses any claim whose class does not exist rather
  than leaving it Pending with the reason in an event.
- Docs: a "Volumes on Oracle Cloud" section (minimums, snapshots, shared volumes through
  File Storage, costs).

### Alternatives

- **Let the CSI driver round up silently** (status quo). Users see a 1Gi volume become
  50Gi after the fact and a surprise on the bill.
- **List and purge detached disks.** Proposed in the first draft for `Retain` classes; the
  cloud classes reclaim with `Delete`, so a deleted volume leaves nothing behind, and the
  safety net before a destructive change is a snapshot. Dropped.
- **Backups through object storage instead of CSI snapshots.** Right for databases
  (RFC-0038) and the platform (RFC-0037); for arbitrary files on a disk the provider's
  snapshot is the fast, consistent tool.
- **`fsGroupPolicy: File` on the OKE CSIDriver** to make NFS volumes writable. Edits an
  object OKE manages and may reset; export options are Oracle's documented path.
- **Keep the mounting process at zero until the restored claim is bound.** Deadlocks with
  `WaitForFirstConsumer` classes, which bind only when a pod mounts the claim.

## Design Details

- `Volume` CRD: `spec.fromSnapshot`; `status.storageClass`, `status.restoredFrom`; phase
  `Restoring`; annotations `shpyrd.io/restore-from` and `shpyrd.io/restore-id`; label
  `shpyrd.io/volume` on VolumeSnapshots. `ResourceStatus.storage` (Postgres, Redis) carries
  the effective size.
- API: `VolumeView.note`, `VolumeView.restoredFrom`; `POST /projects/:slug/volumes` accepts
  `fromSnapshot`; `GET|POST /projects/:slug/volumes/:name/snapshots`, `DELETE
  …/snapshots/:snap`, `POST …/restore {snapshot, to?}` (201 new volume, 202 in place); 501
  where `SHPYRD_SNAPSHOT_CLASS` is empty. `/api/config` exposes `volumes.minSize` and
  `volumes.snapshots` for the dashboard.
- Snapshot support: the `snapshot-controller` component (external-snapshotter v8.6.0 CRDs
  and controller, vendored) and a `VolumeSnapshotClass` from the profile overlay
  (`driver: blockvolume.csi.oraclecloud.com`, `backupType: incremental`, `deletionPolicy:
  Delete`). The server's ClusterRole gains `volumesnapshots` (get, list, watch, create,
  delete); the group exists only where the component is installed.
- Snapshots are labelled with their volume and listed by label; deleting or restoring a
  snapshot through another volume's URL is refused.
- Datastores: `StorageProfile{Class, MinSize}` on the Postgres and Redis reconcilers, from
  the extension's `Deps.Var`. CNPG gets `spec.storage.storageClass`; the Redis claim
  template gets `storageClassName`.
- FSS class: `volumeBindingMode: Immediate`, `reclaimPolicy: Delete`, `encryptInTransit:
  "false"` (in-transit TLS needs `oci-fss-utils` on nodes and port 2051; a node pool
  setting for later), export options `identitySquash: ALL` to uid/gid 0. Only kubelets can
  reach the mount target (its security group admits the workers CIDR; pods have addresses
  in the pods subnet), and a kubelet mounts an export only for a pod whose namespace holds
  the claim, so the squash grants no more than a block volume grants the pod that owns it.
- Terraform: `storage.tf` (`oci_file_storage_mount_target`, `oci_core_network_security_group`
  with TCP 111, 2048-2050 and UDP 111, 2048 from the workers, `oci_identity_policy` in the
  home region). Needs the File Storage service limits `mount-target-count` and
  `file-system-count` above zero in the availability domain; some tenancies start at 0 and
  must request an increase, which is why `shared_storage` defaults to false.

## Open questions

1. Round up to the provider minimum with a message, or refuse sizes below it? Decided:
   round up and say so, before the disk exists.
2. In-place restore as part of the first version? Decided: yes, with confirmation (`--yes`;
   the dashboard offers the new-volume path first and colours the in-place button as
   destructive).
3. Install the snapshot controller on `oci` by default? Decided: yes (OKE ships the CSI
   snapshotter but not the CRDs or the controller).
4. Detached disks? Decided: dropped; cloud classes reclaim with `Delete`, snapshots are the
   safety net.

## Implementation History

- 2026-09-24: RFC written after the OKE proof of concept.
- 2026-09-25: implemented. Verified on OKE: `--size 1Gi` created at 50Gi with the note; a
  non-root process writes to the disk (`fsGroup`); snapshot taken (block volume backup),
  restored into a new volume and in place twice (second restore replaced the disk again);
  Postgres reports `50Gi (5Gi requested; provider minimum)`; kind says snapshots are not
  available and applies no minimum. Shared volumes: Terraform, component and refusals
  built; the PoC tenancy's File Storage limits are 0 in the region, so the `shpyrd-fss`
  class is verified end to end once the limit increase lands.
