# RFC-0026 Web terminal

**Status:** in progress

**Owner:** Marcelo Paez Sequeira (branch `rfc-0026-web-terminal`)

**Depends on:** RFC-0005 (implemented), RFC-0008 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-26

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
- Serving the CLI. RFC-0052 routes `shell` and `run` through the API and will generalise
  this endpoint once it has its own requirements to design against; until then the
  protocol below is shaped for the browser alone.

## Proposal

Three routes. The two HTTP routes sit on the authenticated `/api` group, behind the
session middleware and a role check. The WebSocket does **not**: it is registered on the
public group, so no `s.auth()` runs in front of it and the handler is its own
authorization boundary — it takes the identity from the one-time ticket, re-resolves that
identity's roles itself, and refuses without `project.exec`. That placement is
deliberate, and it is the most security-relevant fact in this design: a browser cannot
set headers on a WebSocket handshake, and `shpyrd cluster dashboard` signs in with a
token in localStorage and so has no session cookie either, so a route requiring either
one would refuse exactly the people the shell is for. Anything added to this handler is
therefore unauthenticated until the ticket is redeemed.

- `GET /api/projects/{slug}/instances` (`project.exec`) lists the instances the selector
  offers: `{name, process, pod, ready}`, named by `logs.InstanceNames` so they match the
  log viewer, and filtered to app processes so build and run instances are absent.
- `POST /api/projects/{slug}/shell/ticket?instance=web.1` (`project.exec`) mints a
  one-time code, held in memory for 30 seconds and bound to the actor, project and
  instance. Being a POST, the existing session middleware already requires the CSRF
  header. Minting is rate limited per actor, since it is the cheapest way to make a
  single-replica control plane do repeated work (two pod LISTs, up to four `pods/exec`
  calls and two audit Events per session), and the number of outstanding tickets is
  capped.
- `GET /api/projects/{slug}/shell?instance=web.1&ticket=...` upgrades to a WebSocket: the
  handshake's `Origin`, when it carries one, must name the host this server answered on,
  the ticket is redeemed once, the role it recorded is checked again, then the server
  opens the exec stream with a TTY and pipes bytes both ways.

Browsers cannot set headers on a WebSocket, which is why the ticket exists and why it
carries the actor rather than leaning on the session cookie; the `Origin` check is the
conventional defence against cross-site WebSocket hijacking and costs nothing, so both
apply. Authorization is checked once, when the socket opens: revoking a grant stops the
next shell from opening but does not kill a shell already running, which ends when the
process exits, the tab closes, or the idle timeout reaps it.

Frames: binary carries terminal bytes in both directions. Text carries JSON control —
`{"type":"resize","cols":N,"rows":N}` from the client, and `{"type":"open","instance":…,
"shell":…}`, `{"type":"exit","code":N}` and `{"type":"error","message":…}` from the
server.

UI: a Shell tab on the project page with an instance selector and xterm.js (fit addon),
"Connecting to web.1", and the exit status when the process ends. The tab appears only
with `project.exec` and is lazy-loaded, so xterm stays out of the main bundle and no
socket opens until it is selected.

Limits: one shell per user per project at a time; idle timeout 30 minutes; every write to
the socket takes a two-second deadline, so a client that stops reading is dropped rather
than holding the session's only writer and with it the reap that would free the user's
slot; resolving the shell is bounded by 15 seconds, since it happens before the idle timer
is armed; inbound frames are capped at 1 MiB; a closed socket cancels the session, so a
process that ignores stdin does not outlive the tab. The server logs and audits
`shell.open` (instance and chosen shell) and `shell.close` (exit code, or the reason it
ended: an exit status, an idle reap, or the client disconnecting).

## Design Details

- `pkg/kexec` grows `StreamIO`: the existing `Stream` without the local terminal, taking
  explicit `io.Reader`/`io.Writer` and a channel of terminal sizes. `Stream` delegates to
  it, so the CLI keeps raw mode and SIGWINCH while the server reuses the same
  WebSocket-with-SPDY-fallback executor.
- The shell is resolved before the terminal opens: the same four candidates in the same
  order, each tried as a cheap non-TTY `kexec.Run` exiting immediately, then one TTY exec
  runs the winner. `shpyrd shell` instead tries them as the interactive session itself and
  reads the error text, which over a live socket risks writing a failed attempt's output
  to the terminal — the race `kexec.CountingWriter` exists to detect. Probing separately
  costs up to four round trips (one for a buildpack image) and keeps every failure
  invisible to the user. The two implementations may drift; the fallback order is the
  contract, not the code.
- The WebSocket authenticates by ticket alone, not by the session cookie: `shpyrd cluster
  dashboard` signs in with a token in localStorage and has no cookie, so a cookie
  requirement would break the shell for exactly the operator most likely to open it. The
  ticket therefore carries the actor, and the roles are resolved again at redemption so a
  grant revoked inside those 30 seconds still takes effect.
- Run pods live in the app's namespace labelled `shpyrd.io/process=run`, so the instance
  listing must exclude that label rather than merely listing the namespace. The
  controller's `runProcess` constant is unexported, so the API package repeats the
  literal; if a third caller needs it, it moves to `api/v1alpha1` instead of being copied
  again.
- The one-shell-per-user registry is in memory, which is correct only because the server
  runs a single replica (`deploy/components/shpyrd/base/server.yaml`). Scaling it out
  requires the limit to move into the control-plane store.
- The idle timer resets on client input, not server output: a chatty process must not hold
  a session open for someone who has walked away.
- Bound by the same network policy and PSS as `shpyrd shell`.
- Same-origin `wss:` satisfies the dashboard's existing `connect-src 'self'`, so the CSP
  is unchanged.
- The exec stream is reached through an injected seam, the way `Options.Apps` and
  `lookupTXT` already are, so role checks, tickets, limits, framing and timeouts are
  tested without a cluster.

## Implementation status

Audited on 2026-09-26 against the code, at the end of the branch's own review. What the
text above promises but the platform does not do yet is listed here.

- **Not implemented:** `closed: server shutting down` is a reachable-looking branch that
  cannot actually be reached, so a control-plane rollout ends every live shell with no
  `shell.close` audited. `srv.Shutdown` neither cancels nor waits for hijacked
  connections, so a shutdown does not cancel the session's context and the handler is
  killed with the process instead of unwinding. Fixing it needs two things this branch
  does not have: a server-lifetime context the bridge can select on, and an audit write
  that outlives the request context, since `pkg/api/audit.go` records through
  `c.Request.Context()`.

## Implementation History

- 2026-09-22: RFC written.
- 2026-09-26: picked up. Settled while designing: an instances endpoint, since the selector
  had no source of instance names; the shell resolved by one probe instead of the CLI's
  retry loop; and an `Origin` check alongside the ticket.
