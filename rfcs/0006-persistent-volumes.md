# RFC-0006 Persistent volumes

**Status:** implemented (text partly superseded) — see Implementation status below

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

A `Volume` project resource creates a PersistentVolumeClaim that processes mount at a path.
Volumes are persistent: they outlive deploys, scale operations and crashes, and are deleted
only explicitly. **ReadWriteOnce** volumes (block storage, available everywhere) pin the
mounting process to a single instance with a Recreate rollout; **ReadWriteMany** sharing
across instances or apps requires the `storage-rwx` extension (a network filesystem) and
comes with a clear "not for SQLite" warning.

## Motivation

Uploads, caches, SQLite databases and tool state need a disk. Kubernetes has the
primitives; users need them without learning access modes and storage classes the hard
way.

### Goals

- `shpyrd volumes create data --size 5Gi`, mount with one line in `shpyrd.yaml`.
- Correct by construction: the platform enforces what the access mode allows.
- Data survives everything except an explicit delete.

### Non-Goals

- Backups/snapshots in the first version (CSI snapshots where supported come next).
- Sharing volumes across projects.

## Proposal

```yaml
# shpyrd.yaml
processes:
  web:
    volumes:
      - name: data          # a Volume resource of the project
        path: /data
```

```
shpyrd volumes create data --size 5Gi [--class <storageclass>] [--shared]
shpyrd volumes list | resize data --size 10Gi | delete data
```

### Access modes, explained to users

- **Single-instance volume** (RWO, default). Block storage attaches to one node at a time.
  The process mounting it is pinned to **1 instance** and rolls out with **Recreate**
  (stop, release the disk, start): a few seconds of downtime per deploy, no data risk.
  Right for SQLite, uploads, caches. `shpyrd scale web=3` on such a process is refused
  with an explanation. Two processes of the same project may mount the same RWO volume
  only if co-located; the platform refuses instead of guessing.
- **Shared volume** (RWX, `--shared`). Needs a provisioner that offers ReadWriteMany:
  the `storage-rwx` extension installs an NFS server provisioner locally (Longhorn or EFS on
  other profiles). Any number of instances and processes can mount it. **SQLite over NFS is
  unsafe** (file locking); the docs and the CLI say so and point to Postgres (RFC-0009) or
  LiteFS/Litestream.

### Persistence

The PVC is owned by the Volume resource, not by any Deployment or pod: deploys, scaling
and restarts never touch it. `shpyrd volumes delete` (and project destroy) delete it,
after confirmation; the reclaim policy is `Retain` for the `dedicated`-like storage
classes where the provisioner supports it so accidental deletes are recoverable by an
admin. Where the bytes live depends on the cluster: cloud disks outlive nodes; kind's
`local-path` keeps them on the node (inside the kind container), so `shpyrd cluster
destroy` deletes them - the one local caveat, documented.

## Design Details

- `Volume` CRD: `spec.size`, `spec.storageClass`, `spec.accessMode` (RWO default, RWX
  when `--shared`), status with phase, capacity, `boundTo` (process types mounting it).
- App controller: for processes with volumes, add `volumes`/`volumeMounts`, set
  `strategy: Recreate` and `replicas: 1` when any mounted volume is RWO (status reason
  "single-instance volume"), and refuse conflicting scale requests in the API/CLI.
- Resize: PVC expansion when the storage class allows it (`allowVolumeExpansion`);
  otherwise an error explaining the limitation.
- Dashboard: Volumes in the project's resource list with size, mode, mounted-by; create
  and resize forms.

### Drawbacks

- Recreate rollouts mean downtime for single-instance volume processes; the alternative
  (rolling update with a RWO disk) deadlocks, so the trade-off is explicit.

## Implementation History

- 2026-09-22: RFC written; RWO volumes in phase B, `storage-rwx` extension in phase E.
- 2026-09-22: Implemented: `Volume` CRD and controller (claim `vol-<name>` owned by the
  Volume, growth through expansion when the storage class allows it, shrink/mode/class
  changes refused, status with capacity and mountedBy), `processes.<type>.volumes` in the
  App (Recreate + 1 instance for ReadWriteOnce, conflicts and over-scaling refused with
  explanations in the controller, API and CLI), `shpyrd volumes create|list|resize|delete`,
  `/api/projects/:ns/volumes` and a Volumes card in the dashboard. Shared (RWX) volumes
  are accepted when the cluster has a provisioner; the local `storage-rwx` extension is
  not written yet. kind's `standard` class does not allow expansion, so resize fails
  there with the explanation.

## Implementation status

Audited on 2026-09-25 against the code. What the text promises but the platform does not do yet is listed here; superseded means a later RFC decided otherwise and the text above is history.

- **Superseded:** Reclaim policy `Retain`: RFC-0060 decided `Delete` on cloud classes with snapshots as the safety net.
- **Not implemented:** The SQLite-over-NFS warning in the documentation (the CLI says it).
