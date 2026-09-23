# RFC-0028 Resource detail pages and metrics

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0009, RFC-0010, RFC-0006 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Clicking a database, cache or volume in the Resources card opens its own page: status
and message, endpoint and the config vars it provides (names only), which apps attach it,
actions (attach/detach, resize, delete, `psql`/`cli` hints) and its **own metrics**:
PostgreSQL from CloudNativePG's exporter, Valkey/Redis from a `redis_exporter` sidecar,
volumes from kubelet volume stats.

## Motivation

Resources are a row in a table today; their health and load are invisible except
through Grafana, which developers may not have.

### Goals

- Every resource kind gets a page from the shared status shape plus kind-specific panels.
- Metrics scraped by the cluster's Prometheus without extra configuration.

### Non-Goals

- Query-level insight (slow query logs, key inspection).

## Proposal

- Route `/projects/<slug>/resources/<kind>/<name>`; API `GET
  /api/projects/{slug}/resources/{kind}/{name}` (status, spec, attachedTo, endpoint,
  provided var names) and `.../metrics?range=`.
- **Postgres**: `spec.monitoring.enablePodMonitor: true` on the CNPG Cluster (the operator
  creates the PodMonitor); panels: connections vs `max_connections`, transactions/s,
  cache hit ratio, replication lag (HA), database size vs storage, WAL rate. CNPG's
  official Grafana dashboard linked for platform roles.
- **Redis/Valkey**: `oliver006/redis_exporter` sidecar on the StatefulSet (metrics port,
  PodMonitor); panels: memory used vs `maxmemory`, hit rate, evictions, connected clients,
  ops/s.
- **Volumes**: `kubelet_volume_stats_used_bytes` vs capacity.
- Actions reuse existing endpoints; delete refused while attached (exists).

## Design Details

- `ext.ResourceType` gains `Metrics []MetricPanel{Title, Query, Unit}` so extensions declare
  their panels; the page renders them generically.
- Instance size and storage shown with "resize" (RFC-0039 for Postgres; Redis resize in the
  same style).

## Open questions

1. Same look as the app Metrics tab (charts, range picker)? Default: yes.
2. Link to the raw Grafana dashboards for platform roles? Default: yes.

## Implementation History

- 2026-09-22: RFC written.
