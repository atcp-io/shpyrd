# RFC-0033 Workspaces

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0008 (implemented), RFC-0016

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

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
- **(b) Environments**: one application with several environments (staging, production),
  each a project, promoted between (`shpyrd promote shop-staging shop`), shared members.
- **(c) Both**, layered: workspace → application → environments.

Default if nothing else is decided: (a), with (b) as a later RFC using promotions built on
the release model.

## Design Details (option a)

- `Workspace` CRD in `shpyrd-system` (owner team, description); label `shpyrd.io/workspace`
  on project namespaces; `ProjectMember` gains `spec.workspace` as an alternative to
  `spec.project`; authz resolves workspace roles into project roles.

## Open questions

1. Which definition: (a), (b) or (c)? Default: (a).
2. Does a workspace need its own domains, quotas or globals in the first version? Default:
   membership inheritance only; the rest follows in the RFCs that introduce them.

## Implementation History

- 2026-09-22: RFC written; blocked on the definition.
