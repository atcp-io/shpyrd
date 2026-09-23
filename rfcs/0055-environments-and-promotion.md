# RFC-0055 Environments and promotion

**Status:** deferred

**Owner:** unassigned

**Depends on:** RFC-0018, RFC-0033

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Staging and production as environments of one application: separate projects grouped
under one name, `shpyrd promote shop-staging shop` re-releasing the exact build (digest)
with production's config, pipelines in the dashboard.

## Motivation

Rebuilding for production risks a different artifact; teams promote what they tested.

### Goals

- Promote by digest, keep each environment's config vars and resources.
- A pipeline view: build once, promote through environments, with who and when.

### Non-Goals

- Pull request preview environments (follow-up).

## Proposal

- `Application` grouping (RFC-0033 option b): `shpyrd environments link shop-staging shop`
  declares the order; `shpyrd promote <from> <to>` pins `<to>`'s image to `<from>`'s current
  release digest (a `Promote from shop-staging v12` release); the registry is shared so no
  copy is needed.
- Guard: refuse promoting a release whose process types are missing in the target;
  optional approval (`project.deploy` on the target).
- Dashboard: Pipeline card on both projects; "Promote" button.

## Decision

Deferred (2026-09-22). Environments follow Git: `main` deploys to the production project,
`dev` to a development project (per-branch auto-deploy, RFC-0018/0054), and a pull request
is the promotion. Revisit if teams need digest promotion without a rebuild.

## Implementation History

- 2026-09-22: RFC written and deferred.
