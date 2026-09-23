# RFC-0019 Health checks and zero-downtime rollouts

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0001 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Configurable HTTP health checks per process (readiness, liveness, startup), connection
draining on shutdown, and a rollout policy that never takes the last healthy instance
away, verified by an automated test that deploys under load and asserts zero failed
requests.

## Motivation

Today a process is "ready" when its port accepts TCP connections; an app that listens but
cannot serve (database down, warm-up) receives traffic. Nobody has proven that rolling
deploys are zero-downtime.

### Goals

- `healthcheck: {path: /healthz}` in `shpyrd.yaml` and instances receive traffic only when
  they answer 200.
- Deploys, scale-downs and restarts never fail requests for a single-instance app.

### Non-Goals

- Canary or blue/green releases.

## Proposal

```yaml
processes:
  web:
    healthcheck:
      path: /healthz          # HTTP GET; default: TCP on the port
      interval: 5s
      timeout: 2s
      gracePeriod: 30s        # startup time allowed before checks count
      shutdownDelay: 5s       # keep serving after SIGTERM is announced
```

- Readiness probe from `path` (or TCP), liveness probe with a higher failure threshold,
  startup probe covering `gracePeriod`.
- Rollout: `maxUnavailable: 0`, `maxSurge: 1`; `preStop` sleep of `shutdownDelay` so the
  endpoint is removed before the process stops; `terminationGracePeriodSeconds` derived.
- Dashboard: health state per instance (passing/failing with the last failure reason);
  `shpyrd projects info` shows "health: HTTP /healthz".
- Verification: an e2e test (`make e2e-rollout`) that runs `hey`/`vegeta` against a demo
  app during a deploy and a `shpyrd scale` and fails on any non-2xx.

## Design Details

- Failing health checks feed the existing failing-instance detection with the probe's
  message ("readiness: GET /healthz 503").
- Volume-pinned processes keep `Recreate` (RFC-0006) and are documented as the exception.

## Open questions

1. TCP stays the default unless a path is configured? Default: yes. No path convention is
   imposed; the examples use `/healthz`.

## Implementation History

- 2026-09-22: RFC written.
