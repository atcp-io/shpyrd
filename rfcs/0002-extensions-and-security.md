# RFC-0002 Extensions, resources and security

**Status:** provisional

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Make the shpyrd core small and extend it through **extensions**: optional platform
capabilities installed like the base stack components (authentication providers, storage
provisioners, database operators, an SSH/shell gateway) and **resource types** that live
inside a project next to the app (Postgres, Redis/Valkey, volumes). Replace the single
admin token with a real identity model (users, teams, roles), enforced in the API and
mirrored into Kubernetes RBAC, and harden the platform for production use. Add Dockerfile
builds next to buildpacks.

This RFC records the options considered and proposes phases; each phase gets its own
issue with the final details.

## Motivation

The MVP (RFC-0001) proves the deploy-to-URL loop with one app per project. Real
workloads need a database, a cache, a disk, a shell into a running instance, and more
than one person with different rights. Doing all of that in the core would make shpyrd
another monolith; doing it as extensions keeps the core reviewable and lets teams pick
what they run.

### Goals

- A single extension mechanism for platform capabilities, used by shpyrd's own addons
  first (authentication, storage, Postgres, Redis, shell) and open to third parties later.
- Projects that hold several resources (app, database, cache, volume) and **bind** them:
  attaching a database to an app injects `DATABASE_URL`, Heroku style.
- Authentication that starts simple (local users) and grows to email/password and OIDC
  (Okta, GitHub, Google) without changing the rest of the system; authorization with
  teams and roles that a small company can actually operate.
- Production-grade security defaults: least privilege, isolation between projects,
  audit trail, secrets never displayed.
- Dockerfile builds for repositories that have one.

### Non-Goals

- A marketplace or remote extension registry (extensions are Go packages compiled into
  the binaries plus manifests, until third-party demand exists).
- Multi-cluster or multi-tenant SaaS control planes.
- Replacing Kubernetes RBAC: shpyrd mirrors into it, it does not reinvent it.

## Proposal

### Extension model

Three options were weighed:

| Option | Pros | Cons |
| --- | --- | --- |
| **A. In-tree extensions**: Go packages behind interfaces, compiled into `shpyrd` and `shpyrd-server`, each with its installer component (Helm/Kustomize) and CRDs, enabled per cluster by configuration | One binary, one release cycle, easy testing, no plugin protocol to version, extensions can share the controller runtime and the UI | Third parties must fork or upstream; every extension is compiled in even when disabled |
| B. Out-of-process plugins (hashicorp/go-plugin, gRPC) | True isolation, independent releases | Plugin protocol to design and version, harder UI integration, more moving parts to operate |
| C. Heroku add-on provider protocol (HTTP provision/deprovision API implemented by any service) | Proven model, language-agnostic providers | Designed for hosted SaaS add-ons; for in-cluster resources a Kubernetes controller is the natural provider anyway |

**Decision: A now, with interfaces that keep B/C possible.** An extension is a Go package
registering into `pkg/ext` with:

- an installer **component** (`deploy/components/<ext>`) added to the profile when the
  extension is enabled (`shpyrd cluster init --enable postgres,shell`), so the runlevel
  engine installs its operators and CRDs with waits like everything else;
- optional **resource types**: a CRD in the `shpyrd.io` group plus a controller-runtime
  reconciler run by `shpyrd-server`, and a **binding** that produces config vars for apps;
- optional **server hooks**: authentication providers, API routes, UI panels (the UI
  discovers enabled extensions from `GET /api/config` and shows their panels);
- optional **CLI commands** (`shpyrd pg`, `shpyrd shell`).

Extension state is recorded in the install record (`shpyrd-system/shpyrd-install`) so the
dashboard can show what is enabled.

### Projects with several resources

Today one project is one `App` in namespace `app-<name>`. The namespace becomes the
**Project**: a `Project` CRD (namespaced object in `shpyrd-system`, or simply the labelled
namespace) carrying domain, team ownership, quotas and the default size; resources are
CRs inside it: `App`, `Postgres`, `Redis`, `Volume`. `shpyrd projects create` keeps
creating the namespace; `shpyrd deploy` keeps targeting the app resource (a project with
one app needs no extra naming; with several, `--resource api`).

**Bindings** are declarative: `spec.bindings: [{ kind: Postgres, name: db }]` on an App
(or `shpyrd attach db`). The App controller collects the bound resources' connection
Secrets and injects their keys (`DATABASE_URL`, `REDIS_URL`) alongside the project's config
vars, which makes attaching a database a config release like any other and lets rollback
carry it.

### Authentication (3.1, 3.2, 3.3)

Options for identity:

| Option | Assessment |
| --- | --- |
| Roll our own users table (bcrypt, sessions, password reset, MFA) | Fine for a handful of local users; a liability once email flows, MFA and account recovery are expected |
| **Bundle an OIDC issuer for local accounts and make the server an OIDC relying party** ([Dex](https://dexidp.io), CNCF; alternatives Keycloak, Zitadel, Ory Kratos/Hydra, Authelia) | One code path for local users, email/password and Okta/GitHub/Google; this is how Argo CD and Weave GitOps do it |
| External only (Okta/Google required) | Blocks local and small-team use |

**Decision:** the server implements one thing, **OIDC relying party** (authorization code
flow with PKCE, cookie session, refresh), plus the existing admin token for bootstrap and
automation. Then:

- **3.1 Basic users** (`auth-local` extension): Dex with its built-in local connector; users
  stored as a Kubernetes Secret of bcrypt hashes managed by `shpyrd users add|passwd|rm`
  and the dashboard. No email infrastructure needed. The admin token stays for `shpyrd`
  automation and break-glass.
- **3.2 Email/password** (`auth-email`): the same Dex, with an SMTP extension for invites,
  verification and password reset, optional TOTP. Or, when a team prefers it, Keycloak as
  the issuer with the same relying-party code.
- **3.3 Okta / any OIDC** (`auth-oidc`): point the server (or Dex as a federating issuer) at
  the provider; groups claims feed teams. Local providers can be disabled.

The CLI keeps using the kubeconfig for cluster operations; `shpyrd login` obtains an OIDC
token (device flow) for API calls that need a user identity (audit, dashboard parity),
and Kubernetes itself can be configured with the same issuer so `kubectl` users are the
same identities.

### Teams, roles and security (3.4)

Model: **Users** (from the identity provider) belong to **Teams** (groups claim or
managed locally). A **Project** is owned by a team; roles are granted to teams (or users)
per project, plus platform-wide roles.

| Role | Sees | Does |
| --- | --- | --- |
| viewer | project overview, releases, builds, logs, metrics; config var **names** | nothing |
| developer | everything viewer sees | deploy, rollback, scale, resize, set/unset config vars, shell into instances, run one-off commands |
| admin (project) | + members, domains, resources | destroy project, attach/detach resources, manage members |
| platform admin | cluster page, extensions, size catalog, all projects | everything, including install/upgrade |

Enforcement happens in two places:

1. **API middleware**: every request is evaluated against (identity, role, project). Denied
   actions are 403, and the UI hides what the role cannot do.
2. **Kubernetes RBAC mirror**: for each project the controller creates `Role`/`RoleBinding`
   objects granting the same verbs to the same OIDC groups, so `kubectl` users see exactly
   what the dashboard would show them. shpyrd never grants wider Kubernetes rights than
   its own service account has.

Production hardening that does not depend on extensions (core work):

- Per-project `NetworkPolicy`: deny cross-project traffic by default; allow ingress from
  ingress-nginx, egress to DNS, the internet and bound resources.
- Pod Security Standards `restricted` on project namespaces; app containers run as non-root
  (buildpack images already do), read-only root filesystem where the stack allows.
- `ResourceQuota` and `LimitRange` per project derived from the plan/size catalog.
- **Audit log**: every mutating API call and CLI action recorded (who, what, when, from
  where) as an append-only stream (Kubernetes Events today, a proper store with the
  storage extension), shown on the project's Activity tab.
- Secrets: values write-only (done); etcd encryption at rest enabled by the installer
  where the cluster allows; config var Secrets and snapshots excluded from backups by
  label; kpack SBOMs kept and images signed with cosign (key managed by the installer).
- Sessions: short-lived cookies, refresh tokens rotated, CSRF token for cookie sessions,
  strict `SameSite`, rate limiting on login and on the API, security headers.
- Supply chain: pinned chart/image digests for the base stack, `cluster status` reporting
  known CVEs from the SBOMs (later).

### Persistent storage (3.5)

A `Volume` resource creates a `PersistentVolumeClaim` and processes mount it
(`processes.web.volumes: [{ name: data, path: /data }]`). Two access modes matter:

- **ReadWriteOnce** works everywhere (kind's local-path, EBS): one node at a time, so a
  process mounting it runs a single instance (`Recreate` strategy, `replicas: 1`
  enforced) and other processes of the same project can share it only on the same node.
  This is the right mode for SQLite; the controller pins the process to one instance and
  warns when scaled.
- **ReadWriteMany** (several apps or instances sharing a mount) needs a provisioner that
  offers it: an NFS server provisioner (`nfs-ganesha-server-and-external-provisioner`,
  simple, fine for local and small clusters) or Longhorn (RWX through an NFS share, with
  snapshots and backups; heavier) locally; EFS on AWS. This is the `storage-rwx`
  extension. SQLite over NFS is unsafe (locking); the docs say so and point to LiteFS or
  Litestream, or to Postgres.

Volumes are project resources (they survive app redeploys), sized like instances
(`1Gi`, `10Gi`), listed and resized from the dashboard, snapshotted where the
provisioner supports it.

### Postgres (3.6)

| Operator | Notes |
| --- | --- |
| **CloudNativePG** | Kubernetes-native, actively maintained, declarative `Cluster` CR, streaming replication, backups to S3-compatible storage (MinIO locally), PITR, connection pooling via PgBouncer `Pooler`, works on arm64 |
| Zalando postgres-operator | Mature, Patroni-based; heavier, older API style |
| Crunchy PGO | Solid, commercial backing, larger footprint |
| StackGres | Feature-rich, heavier |

**Decision: CloudNativePG** as the `postgres` extension. A `Postgres` resource (version,
size from the catalog, storage size, instances 1-3) becomes a CNPG `Cluster`; the binding
exposes `DATABASE_URL` (and `PGHOST`...) from the CNPG-generated app user Secret. Backups
to the object-storage extension (MinIO locally, S3 on AWS). CLI: `shpyrd pg create|psql|
backup|restore`; dashboard: status, connections, size, backups.

### Redis (3.7)

Redis changed its license in 2024 (RSALv2/SSPL, then AGPL); **Valkey** (Linux Foundation
fork, BSD) is the default engine, Redis selectable. Options: a core-managed StatefulSet
(single instance with a PVC, password, `REDIS_URL`; simplest, enough for caches and
queues) or an operator for HA (OT-Container-Kit redis-operator supports Redis and Valkey,
Sentinel/Cluster modes). **Decision:** start with the controller-managed StatefulSet in
the `redis` extension; add the operator path for HA later behind `spec.highAvailability`.

### Shell and one-off commands (3.8)

| Option | Notes |
| --- | --- |
| **`kubectl exec` semantics through the API** (WebSocket to the pod's exec endpoint, xterm.js in the dashboard; CLI uses client-go `remotecommand` directly with the kubeconfig) | What Railway and Render do; no new daemons; audited and role-gated by shpyrd |
| Real SSH gateway (Teleport, sshportal, Fly's hallpass-style sidecar) | Standard SSH clients and tooling; another service to run, key management |
| Web-only terminal | Not enough for `scp`, port-forwards, IDE remote workflows |

**Decision:** `kubectl exec` semantics first: `shpyrd shell [--process web]
[--instance web.2]` attaches to a running instance; `shpyrd run <cmd>` starts a one-off
instance with the release image and config vars (Heroku's `heroku run`), for migrations
and consoles; the dashboard gets a Shell tab. Buildpack run images include `bash`.
`shpyrd forward 5432` port-forwards to bound resources. A Teleport-based `ssh` extension
can come later for teams that need real SSH.

### Dockerfile builds (2)

Buildpacks cannot build application Dockerfiles: CNB "image extensions" only let
Dockerfile snippets modify build and run **base images**, and kpack exposes no
Dockerfile path. Options for a second builder:

| Option | Notes |
| --- | --- |
| **BuildKit rootless** as a Kubernetes Job (`moby/buildkit:rootless`), pushing to the in-cluster registry with plain HTTP | Standard Docker build semantics (multi-stage, secrets, cache mounts), maintained, arm64/amd64, no privileged pods |
| kaniko | No longer maintained by Google (a Chainguard fork exists); rootless without a daemon, but slower and with known Dockerfile gaps |
| Shipwright Build | Nice API, supports both strategies, but pulls in Tekton |
| Build on the developer's machine (`docker build` + push to `localhost:30050`) | Already possible with `--image`; no server-side CD |

**Decision:** BuildKit Jobs orchestrated by the App controller when the source has a
`Dockerfile` (or `build.dockerfile` is set in `shpyrd.yaml`); logs stream like kpack
steps; the resulting digest becomes the release image; layer cache kept in a per-project
PVC or the registry. Buildpacks remain the default when no Dockerfile exists.

## Phases

| Phase | Scope | Notes |
| --- | --- | --- |
| **B** | Dockerfile builds (BuildKit Jobs); `shpyrd shell`, `shpyrd run`, dashboard Shell tab; `Volume` resource with RWO + single-instance enforcement; project model (`Project` + resources, bindings) | No auth changes; unblocks most apps |
| **C** | Extension framework (`pkg/ext`, `--enable`); OIDC relying party in the server; `auth-local` (Dex + local users), `shpyrd login`; audit log | Token stays for automation |
| **D** | Teams, roles, project membership; Kubernetes RBAC mirror; NetworkPolicies, PSS, quotas; `auth-oidc` (Okta) and `auth-email` (SMTP, TOTP) | Production readiness gate |
| **E** | `postgres` (CloudNativePG + object storage extension), `redis` (Valkey), `storage-rwx`; dashboard resource pages; `shpyrd pg`, `shpyrd forward` | Bindings from phase B |
| **F** | AWS profile; published binaries/images; `ssh` (Teleport) extension if demanded | From RFC-0001's roadmap |

Each phase starts with a short design in its GitHub issue (API shapes, UI sketch, test
plan) and ends with docs on shpyrd.io.

## Design Details

Interfaces the extension framework introduces (phase C), kept small on purpose:

```go
// pkg/ext
type Extension interface {
    Name() string
    Component() *install.Component            // installer component, nil if none
    Register(mgr ctrl.Manager) error          // controllers for resource types
    Routes(api gin.IRouter)                   // extra API routes
    CLI() []*cobra.Command                     // extra CLI commands
}

type AuthProvider interface {                  // implemented by auth-local, auth-oidc
    Begin(w http.ResponseWriter, r *http.Request, state string)
    Complete(r *http.Request) (Identity, error)
}

type Binding interface {                       // implemented by Postgres, Redis, Volume
    ConfigVars(ctx context.Context, ref ResourceRef) (map[string]string, error)
}
```

Resource CRDs share a status shape (phase, message, conditions, endpoint) so the
dashboard renders them generically, with per-type panels added by the extension.

### Drawbacks

- In-tree extensions grow the binaries and the dependency graph (operators are still
  separate images; only the reconcilers live in `shpyrd-server`).
- Mirroring roles into Kubernetes RBAC means two places to keep consistent; the controller
  owns both and reconciles drift.
- Dex adds a component to run for local users; it is small and the alternative (own
  password flows) is worse.

## Implementation History

- 2026-09-22: RFC written after the MVP; instance sizes (catalog, `shpyrd sizes`,
  `shpyrd resize`, sizes in releases) landed as the first step toward plans and quotas.
