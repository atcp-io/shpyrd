# RFC-0023 Log drains

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0022a

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Forward a project's logs to any compatible provider: syslog over TLS (RFC 5424) and
HTTPS (JSON batches with custom headers), configured per project like Heroku log drains.

## Motivation

Teams keep their logs in Datadog, Better Stack, Axiom, Papertrail or their own ELK; the
platform should stream to them without an agent in the app.

### Goals

- `shpyrd drains add https://in.logs.example.com/ingest --header "Authorization: Bearer ..."`
  and lines arrive within seconds; several drains per project.
- Delivery status visible (last success, errors).

### Non-Goals

- Provider-specific dashboards or metrics forwarding (RFC-0029 covers OTLP).

## Proposal

- `LogDrain` objects per project (`spec.url`, `spec.headers` from a Secret, `spec.format:
  json|syslog`, `spec.processes` filter); `shpyrd drains add|list|remove`, a Drains card.
- The agent (RFC-0022a, Vector) gets per-project sink components rendered by the controller:
  `otelcol.exporter.otlphttp`/`syslog` for drains; failures (Loki storage is RFC-0022b, optional)
  surface in `status.message` and an audit event after repeated errors.
- Payload: one JSON object per line `{time, project, process, instance, level, message,
  fields}`; syslog: RFC 5424 with the project as APP-NAME and instance as PROCID.

## Open questions

1. Providers to verify explicitly? Default: Better Stack and Papertrail (syslog), Datadog
   and Axiom (HTTPS).

## Implementation History

- 2026-09-22: RFC written.
