# RFC-0022 Log pipeline

**Status:** provisional — see RFC-0022a and RFC-0022b

**Owner:** unassigned

**Depends on:** RFC-0046

**Creation date:** 2026-09-22

**Last update:** 2026-09-23

This RFC was split on 2026-09-23 into two independent parts so the log agent
and drains can ship without first implementing object storage or Loki:

- **RFC-0022a** — Log agent (Vector DaemonSet, container log size limits,
  per-project labelling, feeds drains); no Loki, no object storage required.
  Status: `implementable`.
- **RFC-0022b** — Log storage (Loki as an add-on backed by object storage from
  RFC-0046); enables `--since`, time ranges, "load earlier" in the viewer.
  Status: `provisional` — see original text below for scope and open questions.

The original scope that belongs to RFC-0022b is kept here for reference.

## Original scope (RFC-0022b)

### Summary

Collect every project's logs with an agent and keep them in Loki so they survive
restarts and can be searched over time; the same store keeps run logs (RFC-0024), the
durable audit trail (RFC-0025) and feeds log drains (RFC-0023). A `logs` extension.

### Motivation

Logs live in the container runtime today: a restart or a rollout loses them, one-off runs
lose theirs after ten minutes, and the audit trail expires with Kubernetes Events.

### Goals

- `shpyrd logs --since 7d` and a time range in the viewer.
- Retention configurable; storage bounded.
- Loki is one backend, not the only one; the agent (RFC-0022a) already speaks
  any OTLP/syslog/HTTP target. Loki is an add-on, not a dependency.

### Non-Goals

- Full-text analytics UI beyond the project's viewer (Grafana Explore covers the rest).

### Proposal

- Extension `log-storage`: Loki in single-binary mode receiving from the Vector agent
  (RFC-0022a) via OTLP or Loki's native protocol; object storage from RFC-0046 as the
  backend (MinIO locally, S3 on cloud).
- Server: `GET /api/projects/{slug}/logs?since=&until=&process=` backed by Loki when
  the extension is on; the viewer gains a time range and "load earlier".
- Retention: `SHPYRD_LOGS_RETENTION` (default 7d) and a size cap on the bucket.
- Grafana gets Loki as a data source (behind RFC-0015's sign-in).

### Open questions

1. Retention default 7 days and a 10 GiB cap locally? Default: yes.

## Implementation History

- 2026-09-22: RFC written.
- 2026-09-23: split into RFC-0022a (agent, implementable now) and RFC-0022b
  (Loki/storage, provisional, depends on RFC-0046). Log storage is an optional
  add-on; any OTLP-compatible backend can receive from the agent. RFC-0023 (drains)
  now depends on RFC-0022a only.
