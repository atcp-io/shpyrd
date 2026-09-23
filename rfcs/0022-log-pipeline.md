# RFC-0022 Log pipeline

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0046

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Collect every project's logs with an agent and keep them in Loki so they survive
restarts and can be searched over time; the same store keeps run logs (RFC-0024), the
durable audit trail (RFC-0025) and feeds log drains (RFC-0023). A `logs` extension.

## Motivation

Logs live in the container runtime today: a restart or a rollout loses them, one-off runs
lose theirs after ten minutes, and the audit trail expires with Kubernetes Events.

### Goals

- `shpyrd logs --since 7d` and a time range in the viewer.
- Retention configurable; storage bounded.
- One agent with per-project routing, reusable for forwarding.

### Non-Goals

- Full-text analytics UI beyond the project's viewer (Grafana Explore covers the rest).

## Proposal

- Extension `logs`: Alloy (Grafana's agent) as a DaemonSet reading container logs with
  Kubernetes metadata, labelling by project/process/instance, pushing to Loki (single
  binary mode) with object storage from RFC-0046 (MinIO locally, S3 on cloud).
- Server: `GET /api/projects/{slug}/logs?since=&until=&process=&query=` backed by Loki when
  the extension is on, by the live stream otherwise; the viewer gains a time range and
  "load earlier".
- Retention: `SHPYRD_LOGS_RETENTION` (default 7d) and a size cap on the local bucket.
- Grafana gets Loki as a data source (behind RFC-0015's sign-in).

## Design Details

- Labels: `project`, `process`, `instance` (`web.1` via the pod → instance mapping stored as
  a pod annotation by the controller so the agent can read it), `stream`, `run` for one-off
  pods, `build` for build pods.
- Network policy: project pods need no egress; the agent runs in `logs-system`.

## Open questions

1. Retention default 7 days and a 10 GiB cap locally? Default: yes.
2. Alloy or Vector as the agent? Default: Alloy (same vendor as Loki and Grafana, LogQL
   pipeline stages).

## Implementation History

- 2026-09-22: RFC written.
