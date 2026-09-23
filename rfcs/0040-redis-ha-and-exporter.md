# RFC-0040 Redis high availability and metrics exporter

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0010 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

`spec.highAvailability: true` runs a Redis/Valkey store with a replica and automatic
failover behind a stable endpoint; every store gets a `redis_exporter` sidecar and a
PodDisruptionBudget when replicated. Single instance stays the default.

## Motivation

A restart of the single instance interrupts sessions and queues; production caches expect
failover.

### Goals

- `REDIS_URL` keeps working through a failover without client changes.
- Memory, hit rate, clients and evictions visible (RFC-0028 panels).

### Non-Goals

- Redis Cluster sharding.

## Proposal

- Metrics: `oliver006/redis_exporter` sidecar on the StatefulSet with a PodMonitor (also
  used by RFC-0028). Independent of HA; ships first.
- HA options:
  - **(a) Operator**: OT-Container-Kit redis-operator as the extension's component;
    `RedisReplication` + `RedisSentinel`; clients that speak Sentinel use the Sentinel
    endpoint; for everyone else the controller runs a small HAProxy in front that follows
    the master, so `REDIS_URL` stays one host.
  - **(b) Controller-managed**: a primary and a replica StatefulSet plus the controller
    promoting the replica when the primary is unhealthy, again behind HAProxy. Fewer moving
    parts, more logic to own.
- Default: (a) with the HAProxy front so the URL never changes; `PodDisruptionBudget
  minAvailable: 1`.

## Design Details

- Persistence rules unchanged; HA implies persistence for queues (`persistent: true`
  required when `highAvailability: true`).

## Open questions

1. Operator (a) with a proxy, or controller-managed (b)? Default: (a).
2. Is transparent failover (same URL) a requirement, or is a Sentinel-aware client
   acceptable? Default: transparent.

## Implementation History

- 2026-09-22: RFC written.
