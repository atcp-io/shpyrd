# RFC-0026 Web terminal

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0005 (implemented), RFC-0008 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

A shell into a running instance from the dashboard: xterm.js in a Shell tab, the server
bridging a WebSocket to `pods/exec`, the same launcher/bash/sh fallback as `shpyrd shell`,
gated by the `project.exec` role and audited.

## Motivation

`shpyrd shell` needs the CLI and a kubeconfig; developers with dashboard accounts only
have no shell.

### Goals

- Pick an instance, get a terminal, resize works, sessions end cleanly on tab close.
- Every session audited with actor and instance.

### Non-Goals

- Shells into build or run instances; file transfer.

## Proposal

- `GET /api/projects/{slug}/shell?instance=web.1` upgrades to a WebSocket (cookie session +
  CSRF via a one-time token in the query, since browsers cannot set headers on WebSockets);
  the server opens the exec stream with TTY and pipes bytes both ways; a JSON control
  frame carries resizes.
- UI: Shell tab with an instance selector and xterm.js (fit addon), "Connecting to web.1",
  exit status shown when the process ends.
- Limits: one shell per user per project at a time; idle timeout 30 minutes; the server
  logs and audits `shell.open` and `shell.close`.

## Design Details

- Reuse `pkg/kexec` stream setup with `io.Reader`/`io.Writer` bound to the WebSocket.
- Bound by the same network policy and PSS as `shpyrd shell`.

## Implementation History

- 2026-09-22: RFC written.
