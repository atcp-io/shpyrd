# RFC-0056 Tracing backend (Jaeger)

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0029, RFC-0015

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Traces stored in the cluster with Jaeger and shown per project: the OpenTelemetry
collector (RFC-0029) exports to Jaeger in addition to any external endpoint; a Traces tab
lists recent traces and shows one trace's spans; Jaeger's own UI is available to platform
roles behind shpyrd's sign-in.

## Motivation

Teams without a tracing vendor still want to see a request's path through their services.
Jaeger v2 is built on the OpenTelemetry collector, receives OTLP natively, has a mature UI
and needs no object store for a single-node setup.

### Goals

- Instrumented apps get traces visible in the dashboard with no vendor account.
- Retention bounded by a volume.

### Non-Goals

- Metrics derived from spans; multi-node trace storage (Elasticsearch/ClickHouse are a
  later profile option).

## Proposal

- `tracing` extension: Jaeger v2 (single deployment) with Badger storage on a volume
  (`SHPYRD_TRACING_STORAGE`, default 10Gi, retention 3 days); the RFC-0029 collector gets an
  OTLP exporter to it.
- Server: `GET /api/projects/{slug}/traces?since=&minDuration=` and `/traces/{id}` through
  Jaeger's query API, filtered by the project's service names (set by the collector from
  `service.namespace`).
- UI: Traces tab (list with duration and status, a span waterfall for one trace).
- Jaeger UI proxied at `/jaeger/` for platform roles (it shows every project), reusing the
  session check; not exposed otherwise.

## Design Details

- Sampling and retention are cluster settings; per-project retention later.

## Implementation History

- 2026-09-22: RFC written.
