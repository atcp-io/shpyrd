# RFC-0033 Workspaces

**Status:** in progress (definition being finalised; first pieces shipped)

**Owner:** unassigned

**Depends on:** RFC-0008 (implemented), RFC-0016

**Creation date:** 2026-09-22

**Last update:** 2026-09-26

## Summary

A grouping level above projects with inherited membership, and possibly shared settings
(domains, globals, quotas). The definition is not settled; this RFC records the options and
must be decided before implementation.

## Motivation

Companies group projects by product or team; environments (staging, production) of one
application belong together.

## Proposal

Options:

- **(a) Organisational workspace**: a set of projects owned by a team; roles granted on the
  workspace apply to all its projects; workspace-level globals (RFC-0016), quotas (RFC-0042)
  and domains (RFC-0034); a Workspaces page. Projects can move between workspaces.
- **(b) Environments** grouping was considered and set aside: environments follow Git
  branches deploying to their own projects (RFC-0055, deferred).

Default if nothing else is decided: (a).

## Design Details (option a)

- `Workspace` CRD in `shpyrd-system` (owner team, description); label `shpyrd.io/workspace`
  on project namespaces; `ProjectMember` gains `spec.workspace` as an alternative to
  `spec.project`; authz resolves workspace roles into project roles.

## Open questions

1. Is the organisational grouping (a) wanted at all in the near term? Default: (a), low
   priority.
2. Does a workspace need its own domains, quotas or globals in the first version? Default:
   membership inheritance only; the rest follows in the RFCs that introduce them.

## Implementation status

The definition moved on from "a grouping of projects": the workspace is the tenant every
project, team and person belongs to; the open-source platform has exactly one, implicit,
and behaves as before. The first pieces shipped in v0.4.0:

- A control-plane database (component `control-plane-db`, PostgreSQL in the cluster, or a
  managed one through `SHPYRD_DATABASE_URL`) holding the workspace, the people who signed
  in, teams and project grants — moved out of the `Team`/`ProjectMember` objects, which are
  imported once and marked migrated.
- `GET/PATCH /api/workspace`, `/api/workspace/people`, `/api/workspace/grants`,
  `/api/workspace/export|import` (the platform backup carries the database, RFC-0037).
- A Workspace page in the dashboard (name, people, teams, accounts); the labels
  `shpyrd.io/workspace` on new project namespaces.

Shipped in v0.5.0:

- The `user` role (opens the app; nothing in the builder dashboard) and `spec.access`
  (`public`, `authenticated` — the default for new projects —, `identified`).
- The edge: ingress-nginx `auth_request` to the server for non-public apps; a cookie per
  app host (never the dashboard's session cookie), obtained through a one-time code from
  the dashboard host; `X-Shpyrd-*` headers and an EdDSA JWT (`/.well-known/jwks.json`);
  the "available to team X" page; `Open as` previews; the launcher for `user`-only people;
  `shpyrd access`, `projects create --public`; the admin token as a browser session.

Shipped in v0.6.0:

- Sessions and the edge's one-time codes live in the control-plane database (restarts keep
  people signed in; replicas agree on sign-outs).
- Login methods managed on the Workspace page: Google, Microsoft, GitHub and any OpenID
  Connect provider through the bundled issuer; the join policy (anyone who can sign in /
  only accounts of a claimed domain / only people already in a team); company domain claims
  verified by DNS TXT, routing the domain's accounts to one method.
- The built-in `everyone` team; suspending a person.

Shipped in v0.7.0 and v0.8.0:

- Network allow lists (`allow:` on the project; the Connections card), default closed
  between projects.
- `shpyrd login`, API transport for project commands, the `shpyrd-ctl` operator binary.

Shipped in v0.9.1 — the platform resolves the workspace from the request host:

- `pkg/tenancy`: the `Resolver` interface (host → workspace) with the open-source
  implementation (every host is the one implicit workspace) and a host-based one for
  installs that carry several workspaces, each at an address of its own (`<address>` for
  its dashboard, `<app>.<address>` for its apps). Hosts nobody claims answer "nothing
  here"; suspended workspaces answer so.
- Everything the server does is scoped to the request's workspace: projects (namespace
  `app-<workspace>-<project>` for explicit workspaces, `app-<project>` unchanged for the
  implicit one), teams and grants, sessions (a cookie replayed at another workspace's host
  is anonymous), the edge's JWT (`iss` is the workspace's dashboard URL, `ws` its slug),
  personal tokens, audit, log drains (selected by namespace).
- Reserved names (`www`, `api`, `auth`, `login`, `console`, `shpyrd`, `grafana`, …) are
  refused for new projects; the `capabilities` list in `GET /api/config` tells the
  dashboard and CLI what a server offers beyond the core (empty here).
- Self-hosted installs see no change: one workspace, the same names, the same objects.
  Creating further workspaces is not part of the open-source platform.

Shipped in v0.9.2 — sign-in at a workspace's own host:

- The platform's dashboard is the bundled issuer's one relying party; a workspace at its
  own address sends the browser there to sign in and receives a one-time code back, which
  becomes the workspace's own session (the platform's dashboard keeps none). The
  workspace's join policy and domain claims decide who may enter.
- A reconciler publishes each explicit workspace at its address (Ingress and certificate);
  cluster-level routes and pages (cluster, extensions, global config vars, accounts) belong
  to the operator and do not answer at a workspace's host.
- Explicit workspaces are enforced from birth: no bootstrap mode for them.

Shipped in v0.9.3 — plan limits:

- A workspace may carry ceilings (projects, instances, CPU, memory, storage). The API
  refuses what would exceed them with the number ("this would run 4 instances; the plan
  allows 3"), and the controller backs the check with a `ResourceQuota` per project
  namespace. The Workspace page shows the plan and the usage. Self-hosted installs have no
  ceilings unless one is set.

The rest of the model (OAuth for agents) follows in later releases; the full text is
published when it settles.

## Implementation History

- 2026-09-22: RFC written; blocked on the definition.
- 2026-09-26: definition settled internally; phase 1 (control-plane database, implicit
  workspace, Workspace page) shipped in v0.4.0.
- 2026-09-26: phase 2 (the `user` role, access modes, the edge, previews, launcher) shipped
  in v0.5.0.
- 2026-09-26: phase 3 (sessions in the database, login methods, join policy, domain claims,
  the `everyone` team, suspension) shipped in v0.6.0.
- 2026-09-26: phase 5 (allow lists) shipped in v0.7.0; phase 4 (CLIs) in v0.8.0; phase 6's
  first slice (the workspace resolved from the host) in v0.9.1, its second (sign-in at the
  workspace host through the platform's dashboard) in v0.9.2, its third (plan limits) in
  v0.9.3.
