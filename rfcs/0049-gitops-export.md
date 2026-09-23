# RFC-0049 GitOps export

**Status:** rejected

**Owner:** unassigned

**Depends on:** RFC-0002 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

`shpyrd cluster export` renders the whole base stack, enabled extensions and their
variables into a Git-friendly tree that Flux or Argo CD can apply, kept in step with the
installer so a cluster can be managed either way.

## Motivation

Teams with a GitOps practice want the platform's manifests in their repository, reviewed
and applied by their tooling, not by a CLI with cluster-admin.

### Goals

- Export is complete (CRDs, RBAC, components with values, extensions, hooks' generated
  Secrets as `SealedSecret`/`ExternalSecret` placeholders) and idempotent.
- A documented Flux layout (`Kustomization` per runlevel with `dependsOn`) and an Argo CD
  `ApplicationSet`.

### Non-Goals

- Managing projects and apps through GitOps (the App CRD already allows it; a separate
  guide).

## Proposal

- `shpyrd cluster export -o ./platform --format flux|argocd|plain`: `plain` is today's
  rendering; `flux` adds `HelmRepository`/`HelmRelease` for Helm components and
  `Kustomization` objects per runlevel with dependencies and health checks mirroring the
  installer's waits; `argocd` produces an `ApplicationSet` with sync waves.
- Secrets created by hooks (root CA, admin token, OIDC client) are exported as references
  with instructions (SOPS or External Secrets), never in clear.
- An e2e applies the Flux export to a fresh kind cluster and runs the smoke tests.

## Open questions

1. Flux first, Argo CD second? Default: both in this RFC since the rendering is shared.

## Decision

Rejected (2026-09-22). The platform installs, upgrades and heals itself, and its hooks
generate secrets that do not belong in Git; keeping a second, GitOps-shaped install path in
step with every component change is ongoing cost for a small audience. `shpyrd cluster
export` stays as a plain rendering for inspection and vendoring. Revisit if users ask.

## Implementation History

- 2026-09-22: RFC written and rejected.
