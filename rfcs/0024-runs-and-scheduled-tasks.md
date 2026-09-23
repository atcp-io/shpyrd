# RFC-0024 Run history and scheduled tasks

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0005 (implemented), RFC-0022

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Every one-off command becomes a record with its command, actor, timing, exit code and
retained logs, listed in a Runs tab and in `shpyrd runs`; the same machinery runs
scheduled tasks (`shpyrd schedule add "0 3 * * *" -- rake cleanup`), Heroku Scheduler
style.

## Motivation

`shpyrd run` output is gone once the instance is cleaned up; nothing lists past runs; cron
jobs are the most requested missing primitive after databases.

### Goals

- A run can be started from the dashboard, followed live, and read afterwards.
- Schedules are declared once, visible, and their runs appear in the same history.

### Non-Goals

- Workflow orchestration (dependencies between tasks, retries with backoff).

## Proposal

- `Run` CRD in the project namespace: `spec.command`, `spec.size`, `spec.schedule` (owner);
  `status`: phase, startedAt, finishedAt, exitCode, instance name. `shpyrd run` creates a
  Run instead of a bare pod; the controller creates the pod (same hardening) and mirrors
  its state; logs are read from the pipeline (RFC-0022) or, without it, kept by holding the
  pod for a configurable time (default 24h) instead of ten minutes.
- `Schedule` CRD: `spec.cron`, `spec.command`, `spec.size`, `spec.timezone`, `spec.concurrency:
  forbid|allow`; the controller creates a Run at each tick (a Kubernetes CronJob is not used
  so runs share the Run history and the release's image/config). `shpyrd schedule add|list|
  remove|run-now`.
- Dashboard: Runs tab (list, live output, exit codes), Schedules card with next run time and
  the last result; a "Run command" dialog (developer role).
- Audit: `run.start`, `schedule.add|remove`.

## Design Details

- Runs use the current release's image and config vars at start time (recorded on the Run).
- History pruning: keep the last 50 runs per project plus anything younger than the log
  retention.

## Open questions

1. Schedules in this RFC (default: yes) or a separate one?
2. Without the log pipeline, hold finished run pods for 24 hours? Default: yes.

## Implementation History

- 2026-09-22: RFC written.
