# RFC-0020 Maintenance mode

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0001 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

`shpyrd maintenance on` serves a branded "back soon" page (HTTP 503) at the project's URL
while its processes keep running; `off` restores traffic. Optional custom page and an IP
allowlist so the team can still reach the app.

## Motivation

Migrations and incidents need a way to take an app offline for users without touching
the deployment.

### Goals

- One command or toggle; instant; reversible; visible in the header and audit trail.
- Search engines see 503 with `Retry-After`, not an error page.

### Non-Goals

- Non-HTTP processes (they are unaffected).

## Proposal

- `spec.maintenance: {enabled: true, message: "...", allowFrom: [cidr...]}` on the App;
  `shpyrd maintenance on|off [--message] [--allow 203.0.113.0/24]`; dashboard toggle with the
  same fields.
- The controller creates a per-project `maintenance` Deployment (1 instance,
  `ghcr.io/shpyrd-io/shpyrd-maintenance`, a tiny Go server rendering the page with the
  project's display name, the message and a `Retry-After`) and points the Ingress at it;
  with `allowFrom`, an ingress-nginx allowlist annotation lets those addresses through to the
  real service via a second Ingress.
- Optional custom HTML: `shpyrd maintenance page ./maintenance.html` stored in a ConfigMap.
- Phase badge shows "Maintenance"; releases and scaling still work underneath.

## Design Details

- Switching is an Ingress backend change (no pod restarts); status message "maintenance mode
  since <time> by <actor>".
- The maintenance image is built and published with RFC-0045.

## Open questions

1. Custom HTML per project in this RFC? Default: yes (simple ConfigMap).
2. IP allowlist bypass? Default: yes.
3. Status code 503 with `Retry-After: 3600`? Default: yes.

## Implementation History

- 2026-09-22: RFC written.
