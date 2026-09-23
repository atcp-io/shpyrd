# RFC-0041 Shared volumes (storage-rwx)

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0006 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

A `storage-rwx` extension providing a ReadWriteMany StorageClass: an NFS server and CSI
driver on the local profile, EFS on AWS; shared volumes use it automatically, and
`--shared` is refused with an explanation when no RWX class exists.

## Motivation

`shpyrd volumes create --shared` produces a claim that stays Pending on kind because
nothing provisions ReadWriteMany.

### Goals

- Shared volumes bind and mount from several instances and processes.
- Clear refusal instead of a Pending claim when the extension is off.

### Non-Goals

- Making SQLite safe on a shared volume (documented as unsafe).

## Proposal

- Extension `storage-rwx`: local profile installs `csi-driver-nfs` plus a Ganesha-based NFS
  server backed by a RWO volume, StorageClass `shpyrd-shared` (RWX); AWS profile maps
  `shpyrd-shared` to the EFS CSI driver.
- The Volume controller sets `storageClassName: shpyrd-shared` for `ReadWriteMany` volumes
  when the class exists; otherwise the API and CLI refuse `--shared`: "no shared storage on
  this cluster: enable the storage-rwx extension".
- Docs: sizes and expansion behaviour of the NFS class; SQLite warning stays.

## Design Details

- Verify early that kind nodes (Docker Desktop kernel) can mount NFS; if not, the local
  fallback is `csi-driver-smb` or documenting shared volumes as cloud-only.

## Implementation History

- 2026-09-22: RFC written.
