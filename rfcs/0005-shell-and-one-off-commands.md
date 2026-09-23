# RFC-0005 Shell and one-off commands

**Status:** implemented (CLI); browser terminal → RFC-0026

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Give developers a shell into a running instance (`shpyrd shell`), a way to run one-off
commands with the release's image and config vars (`shpyrd run`, like `heroku run`), a
port-forward to bound resources (`shpyrd forward`), and a terminal tab in the dashboard,
all built on Kubernetes exec semantics, audited and role-gated by shpyrd.

## Motivation

Debugging, migrations, consoles (`rails console`, `python manage.py shell`) and quick
inspections are daily needs. Today the answer is `kubectl exec` with pod names nobody
remembers.

### Goals

- Attach to an instance by process/instance name (`web.2`), not pod hash.
- One-off instances that do not disturb running ones and disappear when done.
- Works from the CLI (kubeconfig) and the dashboard (WebSocket terminal).
- Every session is an audited action (who, which instance, when).

### Non-Goals

- A real SSH server or SSH keys (a Teleport-based extension may come later).
- `scp`/rsync style file transfer (use `shpyrd run` with your own tooling meanwhile).

## Proposal

| Option | Assessment |
| --- | --- |
| **`kubectl exec` semantics through client-go / the API** (chosen) | what Railway and Render do; no new daemons; shpyrd controls authorization and audit |
| SSH gateway (Teleport, sshportal, Fly-style sidecar) | standard SSH clients; another service and key management |
| Web-only terminal | insufficient for scripting |

### Commands

```
shpyrd shell [--process web] [--instance web.2] [-- cmd...]   # attach; default: bash (fallback sh) in the first web instance
shpyrd run <cmd...> [--process worker] [--size shared-s]      # one-off instance with the release image + config vars, TTY, exit code propagated
shpyrd forward 5432[:5432] [--to db]                          # local port -> bound resource endpoint (RFC-0003)
```

`shpyrd run` creates a Pod (not a Job) named `<app>-run-<random>` from the current
release: same image, same config vars and bindings, chosen size, `restartPolicy: Never`,
`activeDeadlineSeconds` 1h, deleted on exit; it counts as an instance `run.1` in logs. It is
what migrations (`shpyrd run npm run migrate`) and consoles use.

### Dashboard

A **Shell** tab per project: instance selector, xterm.js terminal over a WebSocket
(`/api/apps/{ns}/{name}/exec?instance=web.1`), and a "Run command" box for one-offs whose
output streams into the same terminal.

## Design Details

- CLI uses client-go `remotecommand` (SPDY/WebSocket) with the kubeconfig, mapping
  instance names through the same `InstanceNames` logic as logs.
- The server proxies WebSocket ↔ pod exec with the shpyrd ServiceAccount; the request is
  authorized (developer role, RFC-0008) and audited (`Exec` event with user and instance).
  Sessions time out after inactivity; resize messages are forwarded.
- Buildpack run images include `bash`; Dockerfile images may not, so `shell` tries `bash`,
  then `sh`, and reports clearly when neither exists.
- RBAC: `pods/exec` and `pods` create in project namespaces for the server; the mirrored
  Kubernetes roles grant the same to developers (RFC-0008).

### Drawbacks

- Exec sessions bypass the app's own logging; the audit event and the process list in the
  dashboard ("1 shell session") compensate.

## Implementation History

- 2026-09-22: RFC written.
- 2026-09-22: `shpyrd shell` (exec, WebSocket with SPDY fallback, TTY only when stdin is a
  terminal, CNB launcher so buildpack environments load, bash then sh) and `shpyrd run`
  (one-off pod `<app>-run-<rand>` with the release image, config vars and a catalog size,
  attach, exit code passthrough, `--detach`) shipped in the CLI. The controller deletes
  finished one-off pods after 10 minutes. Browser terminal and `pods/exec` through the
  server are not done yet.
