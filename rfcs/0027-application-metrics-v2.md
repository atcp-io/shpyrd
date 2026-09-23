# RFC-0027 Application metrics v2

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0011

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

The Metrics tab gains per-instance series, an instance filter, aggregation (none, sum,
average, max), a percentage/total toggle showing the size's limit, a time range picker and
per-process views. Instances are named `web.1`, never by pod.

## Motivation

The current charts show one line per process; a hot instance or an uneven load balance is
invisible, and tooltips leak pod names.

### Goals

- Compare instances, spot outliers, read absolute values against the allocation.
- Ranges from 15 minutes to 7 days with adequate resolution.

### Non-Goals

- Custom application metrics (RFC-0029) and alerting (RFC-0030 events).

## Proposal

- API `GET /api/projects/{slug}/metrics?range=1h&process=web&by=instance&agg=none|sum|avg|max`
  returns series keyed by instance name (pod → `web.N` through the controller's annotation
  or the existing mapping) for CPU, memory, network, throughput, latency percentiles.
- UI controls as in the reference: Instances (All / one or more), Aggregation, Percentage /
  Total; the limit line ("Limit 256 MiB") drawn on total charts; release markers stay.
- Percentage is relative to the process's requests (memory) as today; total shows bytes
  and cores.
- Time range: 15m, 1h, 6h, 24h, 7d with step chosen by range.

## Design Details

- PromQL: `container_memory_working_set_bytes{namespace, pod=~"<deployment>-.*"}` grouped by
  pod, aggregated server-side with `sum by`, `avg by`, `max by`; instance names resolved via
  the pod list.
- Remove pod names from every tooltip and legend (RFC-0011).

## Implementation History

- 2026-09-22: RFC written.
