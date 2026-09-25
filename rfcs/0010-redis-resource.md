# RFC-0010 Redis resource

**Status:** implemented (with gaps) — see Implementation status below

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

A `Redis` project resource for caches and queues (`redis` extension), running **Valkey**
by default (Redis selectable), as a controller-managed StatefulSet first and through an
operator for high availability later; attached to apps as `REDIS_URL`.

## Motivation

Caches, sessions, rate limits and job queues (Sidekiq, Celery, BullMQ) need Redis-compatible
storage; it is the second most requested addon after a database.

### Goals

- `shpyrd redis create cache` to `REDIS_URL` in one command.
- Persistent or ephemeral (cache-only) modes; a size from the catalog.
- HA when needed without changing the resource API.

### Non-Goals

- Redis Cluster sharding in the first version.

## Proposal

**Engine.** Redis changed its license in 2024 (RSALv2/SSPL, later AGPL for 8.x). **Valkey**
(Linux Foundation fork, BSD) is protocol-compatible and the default engine; `engine: redis`
selects upstream Redis for teams that need it.

| Implementation | Assessment |
| --- | --- |
| **Controller-managed StatefulSet** (chosen first) | single instance, PVC when persistent, password in a Secret, `REDIS_URL`; simple and enough for caches and queues |
| OT-Container-Kit redis-operator | Redis and Valkey, Sentinel and Cluster modes; adds HA behind `spec.highAvailability: true` later |
| Spotahome redis-operator | less active |

```yaml
apiVersion: shpyrd.io/v1alpha1
kind: Redis
metadata: { name: cache, namespace: app-shop }
spec:
  engine: valkey          # or redis
  version: "8"
  size: shared-s
  persistent: false       # true: AOF on a PVC
  storage: 1Gi
  highAvailability: false # later: Sentinel via the operator
status:
  phase: Ready
  endpoint: cache.app-shop.svc:6379
```

The `Binding` exposes `REDIS_URL` (`redis://:password@host:6379/0`) and `REDIS_HOST`,
`REDIS_PORT`, `REDIS_PASSWORD`. CLI: `shpyrd redis create|list|info|cli|delete`.

## Design Details

- StatefulSet with the size's resources, `maxmemory` derived from the memory allocation
  with `allkeys-lru` for cache mode, `requirepass` from a generated Secret, TLS off inside
  the project network (network policies from RFC-0008 isolate it), `PodDisruptionBudget`.
- Metrics via `redis_exporter` sidecar into Prometheus; a small dashboard panel (hit rate,
  memory, clients).
- Deletion refuses while bound; persistent data follows RFC-0006 rules.

### Drawbacks

- Single instance means a restart drops in-memory data in cache mode; that is the expected
  behaviour of a cache and is stated in the UI. HA comes with the operator path.

## Implementation History

- 2026-09-22: RFC written; phase E.
- 2026-09-22: Implemented as the `redis` extension (no operator component): a `Redis` CRD
  (engine valkey|redis, version, size, persistent, storage) run by the controller as a
  single-replica StatefulSet with a generated password, `maxmemory` at 75% of the size's
  memory, `allkeys-lru` for caches or AOF on a volume with `noeviction` when persistent,
  uid 999 with the restricted security context; a `Binder` exposing
  `REDIS_URL|HOST|PORT|PASSWORD`; `shpyrd redis create|list|info|cli|delete`. Persistence
  cannot change after creation. Not done: Sentinel/HA through an operator, the exporter
  sidecar and dashboard panel, PodDisruptionBudget.

## Implementation status

Audited on 2026-09-25 against the code. What the text promises but the platform does not do yet is listed here; superseded means a later RFC decided otherwise and the text above is history.

- **Not implemented:** A PodDisruptionBudget for the Redis instance.
