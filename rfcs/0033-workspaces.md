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

The rest of the model (many workspaces, identity per workspace, the `user` role and the
edge) follows in later releases; the full text is published when it settles.

## Implementation History

- 2026-09-22: RFC written; blocked on the definition.
- 2026-09-26: definition settled internally; phase 1 (control-plane database, implicit
  workspace, Workspace page) shipped in v0.4.0.
