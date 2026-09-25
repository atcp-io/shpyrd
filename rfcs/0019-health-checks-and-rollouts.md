# RFC-0019 Health checks and zero-downtime rollouts

**Status:** implemented (with gaps) — see Implementation status below

**Owner:** Patrick Negri (shpyrd-io/shpyrd main)

**Depends on:** RFC-0001 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-23

## Summary

Configurable HTTP health checks per process (readiness, liveness, startup), connection
draining on shutdown, and a rollout policy that never takes the last healthy instance
away, verified by an automated test that deploys under load and asserts zero failed
requests.

## Motivation

Today a process is "ready" the moment its container starts; an app that is still warming
up or waiting for a database connection receives traffic immediately. A deploy that
crashes on startup still rolls forward. Nobody has proven that rolling deploys keep
traffic flowing.

### Goals

- `healthcheck: {path: /healthz}` in `shpyrd.yaml` and instances receive traffic only when
  they answer 200.
- Deploys, scale-downs and restarts never fail requests for a single-instance app.

### Non-Goals

- Canary or blue/green releases.

## Proposal

**Defaults (no shpyrd.yaml needed):**

| Process type | Default probe |
|---|---|
| `web` | HTTP `GET /` on `PORT` |
| Any process with `port:` set explicitly | TCP on that port |
| Everything else (worker, scheduler…) | **none** — workers don't bind a port; rely on restart-on-crash |

Custom checks override the default:

```yaml
processes:
  web:
    healthcheck:
      path: /healthz          # HTTP GET on PORT; removes the default / check
      interval: 5s
      timeout: 2s
      gracePeriod: 30s        # startup time allowed before checks count
      shutdownDelay: 5s       # keep serving after SIGTERM is announced
  worker:
    healthcheck:
      command: [python, -c, "import app; app.is_healthy()"]
```

`tcp: true` is the explicit TCP check when a non-web process binds a port.

- **Probes**: readiness probe (removes from service), liveness probe (restarts the
  instance), startup probe covering `gracePeriod`. The liveness probe has a higher failure
  threshold than readiness so a slow-to-warm instance is removed before it is killed.
- **Rollout**: `maxUnavailable: 0`, `maxSurge: 1` (one extra instance during rollout,
  full count always serving). Exception: volume-pinned processes keep `Recreate`.
- **Shutdown**: `preStop` sleep of `shutdownDelay` (default 5 s) gives the load balancer
  time to drain; `terminationGracePeriodSeconds = shutdownDelay + timeout + 5` to avoid
  a SIGKILL mid-drain.
- **Failure surfacing**: a failing health check feeds the existing "failing instance"
  detection with the probe's last message ("readiness: GET /healthz 503"). The Activity
  panel shows the reason and a rollback button when the rollout stalls. `shpyrd projects
  info` shows the health config ("health: HTTP /healthz every 5s").
- **Verification**: `TestRolloutZeroDowntime` in `internal/controller` starts a deploy,
  pumps requests through the service, and asserts no connection-refused or 5xx.

## Design Details

- Failing health checks feed the existing failing-instance detection with the probe's
  message ("readiness: GET /healthz 503").
- Volume-pinned processes keep `Recreate` (RFC-0006) and are documented as the exception.

## Settled questions

1. **Worker probes**: workers have no default probe; one is added explicitly via `command:`
   or by setting `port:`. TCP as a universal default was rejected: workers don't bind
   ports, it would always fail and block the rollout.
2. **Web default path**: `GET /`, not `/healthz`. Any app that listens serves `/` (even a
   404 is from the app, not from a crashed process); `/healthz` requires extra framework
   code and is opt-in.
3. **Rollout strategy**: `maxUnavailable: 0, maxSurge: 1` for all multi-instance
   processes. Singleton workers (where two concurrent instances are unsafe) can force
   Recreate by mounting a RWO volume; a `singleton: true` shorthand is a follow-on.

## Implementation History

- 2026-09-22: RFC written.
- 2026-09-23: probe defaults settled; implemented in shpyrd-io/shpyrd (commit f02265a).

## Implementation status

Audited on 2026-09-25 against the code. What the text promises but the platform does not do yet is listed here; superseded means a later RFC decided otherwise and the text above is history.

- **Not implemented:** The zero-downtime rollout test (`TestRolloutZeroDowntime`).
- **Not implemented:** The probe's own message ("GET /healthz 503") in failing-instance status; users see "readiness probe failing".
