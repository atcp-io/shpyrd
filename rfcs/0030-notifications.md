# RFC-0030 Notifications

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0013 (email channel only)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Platform events delivered to webhooks (signed JSON), Slack (incoming webhooks) and email:
deploys succeeded or failed, builds failed, instances failing, rollbacks, resources ready,
membership changes. Subscriptions per project and cluster-wide.

## Motivation

People learn about a failed deploy by looking at the dashboard.

### Goals

- `shpyrd notify add slack https://hooks.slack.com/... --events deploy.failed,instances.failing`
  and messages arrive within seconds.
- Events are the same the audit trail records plus state transitions the controller sees.

### Non-Goals

- Paging/on-call integrations (PagerDuty) beyond a webhook; alert rules on metrics.

## Proposal

- `Notification` objects (project namespace or `shpyrd-system`): `spec.channel:
  webhook|slack|email`, `spec.target` (URL or addresses; secrets in a Secret), `spec.events`
  (list or `*`).
- Event source: a small in-server bus fed by audit entries (`deploy`, `rollback`, `scale`,
  `member.*`) and by the App controller's phase transitions (`deploy.succeeded`,
  `deploy.failed`, `build.failed`, `instances.failing`, `resource.ready`).
- Delivery: webhook POST with `X-Shpyrd-Signature` (HMAC-SHA256 of the body with the
  channel's secret), retries with backoff, last delivery status on the object; Slack Block
  Kit message with a link to the project; email via RFC-0013.
- CLI `shpyrd notify add|list|remove|test`; dashboard Notifications card (project) and
  Cluster card (platform-wide).

## Open questions

1. First events: `deploy.succeeded`, `deploy.failed`, `build.failed`, `instances.failing`,
   `rollback`? Default: yes, plus `resource.ready` and `member.*`.
2. Slack through incoming webhooks (no Slack app)? Default: yes.

## Implementation History

- 2026-09-22: RFC written.
