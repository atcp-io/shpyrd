# RFC-0029 OpenTelemetry

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0016

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

An `otel` extension: an OpenTelemetry Collector receiving OTLP from apps, with
`OTEL_EXPORTER_OTLP_ENDPOINT` and resource attributes injected automatically, exporting to
an external OTLP endpoint (Honeycomb, Grafana Cloud, Datadog, any vendor) and, for metrics,
into the cluster's Prometheus.

## Motivation

Instrumented apps need somewhere to send traces and metrics; configuring an exporter per
app is repetitive, and the platform already knows the service name and version.

### Goals

- An app using any OTel SDK exports without configuration; the release is the
  `service.version`.
- One place to point the cluster at a vendor.

### Non-Goals

- A local tracing backend and UI (Tempo + a Traces tab): a follow-up RFC.

## Proposal

- Collector Deployment (`otel-system`) with OTLP gRPC/HTTP receivers, batch processor,
  exporters: `otlphttp` to `SHPYRD_OTEL_ENDPOINT` with headers from a Secret, and
  `prometheusremotewrite` into the cluster Prometheus for metrics.
- Injection through global config vars (RFC-0016): `OTEL_EXPORTER_OTLP_ENDPOINT`,
  `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`; per-app `OTEL_RESOURCE_ATTRIBUTES` and
  `OTEL_SERVICE_NAME` set by the App controller (project, process, release).
- `shpyrd otel set --endpoint https://api.honeycomb.io --header "x-honeycomb-team=..."`;
  Cluster card with status and throughput.

## Design Details

- Network policy: project pods reach the collector as a platform namespace (allowed).
- Sampling configurable (`SHPYRD_OTEL_SAMPLE_RATIO`).

## Open questions

1. Local tracing backend later? Default: yes, follow-up RFC once this lands.

## Implementation History

- 2026-09-22: RFC written.
