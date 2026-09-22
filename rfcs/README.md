# RFCs

Design changes to shpyrd are proposed as short RFCs before they are built.

## Process

1. Discuss the idea in a GitHub issue or on Discord.
2. Copy `0000-template.md` to `NNNN-title.md`, fill it in and open a pull request.
3. Address feedback with additive commits; the RFC's status moves from `provisional` to
   `implementable` when maintainers agree, and to `implemented` when the code lands.
4. Keep the Implementation History section current.

## Index

| RFC | Title | Status |
| --- | --- | --- |
| [0001](0001-mvp-local-platform.md) | MVP: local platform, App CRD and CLI | implemented |
| [0002](0002-extension-model.md) | Extension model | implemented (framework) |
| [0003](0003-projects-and-resources.md) | Projects and resources | implemented (bindings plumbing) |
| [0004](0004-dockerfile-builds.md) | Dockerfile builds (BuildKit) | implemented |
| [0005](0005-shell-and-one-off-commands.md) | Shell and one-off commands | implemented (CLI) |
| [0006](0006-persistent-volumes.md) | Persistent volumes | implemented (RWO) |
| [0007](0007-authentication.md) | Authentication (OIDC, Dex, Okta) | implemented (3.1 local users) |
| [0008](0008-teams-roles-and-security.md) | Teams, roles and security | implemented (quotas, supply chain pending) |
| [0009](0009-postgres-resource.md) | Postgres resource (CloudNativePG) | provisional |
| [0010](0010-redis-resource.md) | Redis resource (Valkey) | provisional |

## Phases

| Phase | RFCs | Scope |
| --- | --- | --- |
| B | 0005, 0004, 0006, 0003 | shell and one-off commands; Dockerfile builds; RWO volumes; project resources and bindings |
| C | 0002, 0007 | extension framework; OIDC relying party; local users (Dex) |
| D | 0008, 0007 | teams and roles, RBAC mirror, isolation, audit; Okta and email/password |
| E | 0009, 0010, 0006 | Postgres, Redis/Valkey, shared (RWX) storage |
| F | 0001 roadmap | AWS profile, published binaries |
