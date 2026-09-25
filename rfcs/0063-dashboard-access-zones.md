# RFC-0063 Dashboard access zones: public dashboard, intranet-only areas

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0036 (front doors), RFC-0008 (roles), RFC-0035 (cloud profiles: VPN)

**Creation date:** 2026-09-25

**Last update:** 2026-09-25

## Summary

Today the dashboard is either public or, with `--platform-exposure internal`, entirely
behind the internal front door (VPN only). Both are blunt: a public dashboard exposes
sign-in and administration to the internet; an internal one also hides the parts a
team member on the road legitimately needs (a project's status, logs, a rollback). This
RFC keeps the dashboard reachable from the internet and lets a platform administrator
mark **areas** as intranet-only from the settings page: requests to those areas are
accepted only from the internal network (the VPC, the VPN clients), everything else
stays public.

## Motivation

Operators want the platform's administration (users, teams, the cluster page, shells)
reachable only from the office or the VPN, while developers want to check a deploy from a
phone. Moving the whole dashboard is the only knob, and it also moves sign-in, which
breaks browser flows for everyone outside.

### Goals

- A setting (dashboard and `shpyrd cluster settings`) with the areas and their zone:
  `public` or `intranet`.
- Enforcement in the server, by source address against the configured intranet ranges,
  with the front door's forwarded address trusted only when it comes from the load
  balancer.
- Clear refusals: "this area is available from the company network or the VPN".

### Non-Goals

- Identity-aware proxies and device posture (a VPN or IAP is the operator's choice).
- Per-project zones (a project's exposure already exists: RFC-0036).

## Proposal

- **Areas** (the coarse map of the dashboard and API): `sign-in` (the login page and
  callbacks), `projects` (read), `deploys` (deploy, rollback, scale, config), `shells`
  (exec/attach, one-off commands), `administration` (users, teams, sizes, extensions,
  cluster page, registry), `api-tokens` (RFC-0031 when it lands). Each is `public` or
  `intranet`; the default is everything public, matching today.
- **Intranet ranges**: the profile knows the VPC/VCN range and the VPN client range from
  the infrastructure (`SHPYRD_INTRANET_CIDRS`, printed by Terraform); the settings page
  shows them and allows additions (an office address).
- **Enforcement**: the server resolves the client address from the connection or from
  `X-Forwarded-For` when the immediate peer is the ingress controller (which sets it from
  the load balancer's client address: pod-target NLBs and OCI load balancers preserve it),
  matches it against the ranges, and answers 403 with the explanation above for an
  intranet area. The CLI gets the same answer and prints it.
- **Sign-in as an area**: marking `sign-in` intranet means nobody signs in from outside,
  while existing sessions keep the public areas working until they expire; the settings
  page warns before saving that this locks out remote sign-in.
- **Relation to `--platform-exposure internal`**: unchanged and still available for
  operators who want no public surface at all; the two compose (an internal dashboard has
  every area intranet by construction).

### Alternatives

- **Two dashboards** (public read-only, internal full). Two hostnames, two sessions,
  confusing.
- **ingress-nginx `whitelist-source-range` per path.** Static, per-Ingress, and paths do
  not map cleanly to areas (the API is one prefix); the server knows the areas.

## Design Details

- Settings live in a ConfigMap in `shpyrd-system` (`dashboard-access`), read by the server
  with a short cache; `GET/PUT /api/cluster/access` (cluster.admin).
- Each protected route declares its area next to its action (RFC-0008's `require`), so
  the map is one table.
- The client-address resolution trusts `X-Forwarded-For` only from the ingress
  controllers' pod range; on kind everything is intranet (127.0.0.1).
- Audit: changes to the areas are cluster audit events; refusals are counted, not logged
  per request.

## Open questions

1. Default zone for `administration` on cloud profiles: `intranet` when a VPN exists?
   Default: public, until an operator changes it (no surprise lockouts).
2. Should the setting also apply to the CLI's server-proxied calls? Default: yes, the
   server does not distinguish.

## Implementation History

- 2026-09-25: RFC written while moving the dashboard behind the internal front door on
  AWS.
