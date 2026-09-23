# RFC-0042 Project quotas

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0008 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Per-project ceilings on total CPU, memory, storage and instances, set by platform admins,
enforced by Kubernetes `ResourceQuota`, checked upfront by the API and CLI so users get
"this would exceed the project's 8 GiB" instead of pods stuck Pending, and shown as usage
bars on the project page.

## Motivation

Instance sizes shape one instance; nothing caps how many a project runs or how much
storage it claims.

### Goals

- `shpyrd projects quota shop --cpu 4 --memory 8Gi --storage 50Gi --instances 20`.
- Scale, resize, volume and database creation refused with the exact number that would be
  exceeded.

### Non-Goals

- Plans or billing; cluster-wide fairness beyond per-project caps.

## Proposal

- `ResourceQuota shpyrd` in the project namespace (`requests.cpu`, `requests.memory`,
  `requests.storage`, `count/pods`) rendered by the controller from an annotation set on
  the App (`shpyrd.io/quota`) or, later, on the workspace (RFC-0033).
- A `LimitRange` with **maximums only** (no defaults: build pods set requests without limits
  on purpose).
- API/CLI pre-checks: compute requested totals from sizes and counts and compare with the
  quota's hard limits and current usage; the controller also surfaces quota rejections in the
  status ("quota: memory 8Gi exceeded by 512Mi").
- Dashboard: usage vs quota bars (from `ResourceQuota.status`); Cluster page: quotas per
  project.
- Default quotas per cluster: `SHPYRD_DEFAULT_QUOTA_*` vars applied to new projects.

## Design Details

- Build pods (BuildKit, kpack) count against the quota only while running; documented.

## Implementation History

- 2026-09-22: RFC written.
