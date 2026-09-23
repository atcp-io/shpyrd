# RFC-0047 Autoscaling

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0019, RFC-0042

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

A process type runs either with a fixed instance count (today) or in **autoscaling mode**
with a minimum and a maximum: the count follows CPU or memory utilisation
(HorizontalPodAutoscaler) or, through a `keda` extension, queue depth and custom metrics.
The dashboard shows "3 instances (auto 2-10)".

## Motivation

Traffic varies; fixed instance counts either waste money or fall over.

### Goals

- `autoscale: {min: 2, max: 10, cpu: 70}` per process and the count follows load.
- Queue-driven workers scale on backlog (Redis list length, Postgres query, Prometheus).

### Non-Goals

- Scale-to-zero for web processes (cold starts; a later RFC if wanted); cluster autoscaling
  (the cloud profile's node groups).

## Proposal

- `processes.<type>.autoscale: {min, max, cpu?, memory?, triggers?: [...]}`; the controller
  renders an HPA (CPU/memory targets against requests) or, when `triggers` are set, a KEDA
  `ScaledObject` (`keda` extension installs the operator).
- `shpyrd scale web=3` on an autoscaled process sets `min` (and warns); `shpyrd autoscale
  web --min 2 --max 10 --cpu 70`; dashboard shows the live count, the bounds and the current
  metric; single-instance volume processes cannot autoscale (refused with the RFC-0006
  explanation).
- Quotas (RFC-0042) bound `max`.

## Design Details

- HPA behaviour: scale-up fast, scale-down stabilisation 5 minutes; metrics-server is added
  to the base stack (the local profile ships it; cloud profiles usually have it).
- KEDA triggers first: Redis list length (attached Redis), Prometheus query, cron.

## Implementation History

- 2026-09-22: RFC written.
