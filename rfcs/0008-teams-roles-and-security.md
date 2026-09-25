# RFC-0008 Teams, roles and security

**Status:** implemented (with gaps) — see Implementation status below

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Authorization and hardening for production: users belong to **teams**, projects are owned
by teams, **roles** (viewer, developer, admin, platform admin) say what each can see and
do; enforcement in the API and a **mirror into Kubernetes RBAC** so `kubectl` users see the
same. Plus the core security work that does not depend on extensions: isolation between
projects, quotas, an audit log, hardened sessions and supply chain.

## Motivation

Identity (RFC-0007) without authorization only puts names on an all-powerful token. A
production platform needs least privilege, tenant isolation and a record of who did what.

### Goals

- A small role set a company can operate without a policy language.
- The dashboard hides what the role cannot do; the API refuses it; Kubernetes agrees.
- Projects cannot see or reach each other unless allowed.
- Every mutation is attributable.

### Non-Goals

- Fine-grained per-resource ACLs or custom roles (revisit with demand).
- Multi-cluster identity federation.

## Proposal

### Model

Users (from the identity provider) belong to **Teams** (groups claim, or managed locally).
A project is owned by one team; roles are granted per project to teams or users. Two
platform-wide roles exist besides.

| Role | Sees | Does |
| --- | --- | --- |
| viewer | project overview, releases, builds, logs, metrics; config var **names** | nothing |
| developer | everything viewer sees | deploy, rollback, scale, resize, set/unset config vars, shell, one-off commands |
| admin (project) | + members, domains, resources | destroy project, attach/detach resources, manage members |
| platform admin | cluster page, extensions, size catalog, all projects | everything, install/upgrade |
| platform viewer | cluster page read-only, all projects read-only | nothing |

### Enforcement

1. **API middleware**: every request resolves (identity, project, action) → allow/deny;
   denied is 403 and the UI hides the action beforehand (roles are part of
   `GET /api/me`).
2. **Kubernetes RBAC mirror**: per project the controller maintains `Role`/`RoleBinding`
   objects granting the equivalent verbs to the OIDC groups (viewer: get/list/watch on
   the namespace's objects; developer: + update App spec, create pods/exec; admin: +
   delete). shpyrd never grants more than its own ServiceAccount has. Drift is
   reconciled.

### Core hardening (no extension needed)

- **Network isolation**: default-deny `NetworkPolicy` per project namespace; allow ingress
  from ingress-nginx, egress to DNS, the internet and bound resources; projects reach each
  other only through a declared binding.
- **Pod security**: `restricted` Pod Security Standard on project namespaces; app
  containers non-root (buildpack images already are), no privilege escalation; Dockerfile
  images that need root are refused with an explanation.
- **Quotas**: `ResourceQuota` and `LimitRange` per project derived from a plan (sum of
  sizes) so one project cannot starve the cluster.
- **Audit log**: every mutating API call and CLI action (deploy, rollback, scale, resize,
  config change, shell, destroy, login) recorded as `{who, what, target, when, from}`,
  append-only, shown on the project's Activity tab and exportable; Kubernetes Events until
  the storage extension provides a durable store.
- **Secrets**: values write-only (done); etcd encryption at rest enabled by the installer
  where the profile allows; config var Secrets excluded from backups by label; kpack SBOMs
  retained; images signed with cosign and verified at deploy (later).
- **Sessions and API**: short cookies with rotation, CSRF for cookie mutations, rate limits
  on login and API, security headers (HSTS, CSP for the dashboard), admin token rotation.
- **Supply chain**: base stack charts and images pinned by digest; `cluster status` reports
  drift and known CVEs from SBOMs (later).

## Design Details

- `pkg/authz`: `Can(identity, action, project) bool` with a static role → actions table;
  actions are the API's verbs (`app.deploy`, `app.rollback`, `config.set`, `shell.exec`,
  `project.destroy`, `cluster.admin`...).
- Membership objects: `Team` (name, members, groups claim) and `ProjectMember`
  (project, subject or team, role) as CRDs in `shpyrd-system`, editable from the CLI
  (`shpyrd teams`, `shpyrd members`) and the dashboard.
- The RBAC mirror uses `ClusterRole`s `shpyrd-viewer|developer|admin` (aggregated) and
  per-namespace `RoleBinding`s to groups.

### Drawbacks

- Two enforcement points (API and Kubernetes RBAC) must stay consistent; the controller
  owns both from one table. Network policies can surprise apps that call each other
  across projects; the binding path is the supported way.

## Implementation History

- 2026-09-22: RFC written; phase D.
- 2026-09-22: Implemented. `pkg/authz` holds the static role table (`Roles.Can(action,
  project)`) and resolves roles from cluster-scoped `Team` (members, identity-provider
  groups, optional platform role) and `ProjectMember` (project, role, user or team)
  objects, cached for five seconds. Bootstrap rule: with no Team or ProjectMember every
  signed-in user is a platform admin, and the admin token always is. Every protected API
  route names its action; refusals are 403 with the role and the verb; project lists are
  filtered; `/api/me` carries the roles the dashboard hides actions by. `/api/teams` and
  `/api/projects/{ns}/members` plus `shpyrd teams` and `shpyrd members`. The membership
  controller mirrors grants into `RoleBinding`s (`shpyrd-viewer|developer|admin` to the
  fixed ClusterRoles `shpyrd-project-*`) per project namespace and `ClusterRoleBinding`s
  for platform roles, subjects being user emails and group names, so an API server
  configured with the same OIDC issuer gives kubectl users the same view. Hardening: a
  `NetworkPolicy` per project (ingress from the project, ingress-nginx and monitoring;
  egress to the project, non-project namespaces and the internet minus the pod CIDR when
  known), `restricted` PSS labels in warn/audit mode, app and one-off containers non-root
  with all capabilities dropped and RuntimeDefault seccomp (named Dockerfile users are
  refused with an explanation: the kubelet verifies numeric users only), security headers
  and a strict CSP, a per-client rate limit on sign-in, `shpyrd cluster token --rotate`.
  Audit: API mutations and CLI actions are recorded as Kubernetes Events with structured
  annotations (`{who, what, target, detail, from, via}`), listed by
  `GET /api/apps/{ns}/{name}/audit` and on the project page. Not done: ResourceQuota and
  LimitRange (they need a plan model; a LimitRange default would starve build pods),
  etcd encryption, cosign, SBOM/CVE reporting, per-user API tokens, and enforce-mode PSS
  (BuildKit build pods need seccomp/AppArmor exemptions).

## Implementation status

Audited on 2026-09-25 against the code. What the text promises but the platform does not do yet is listed here; superseded means a later RFC decided otherwise and the text above is history.

- **Fixed:** 2026-09-25: the Kubernetes RBAC mirror bound developers and admins only to their incremental ClusterRole; the roles are aggregated now (developer includes viewer, admin includes both).
- **Not implemented:** Domains are gated by `project.config` (developers) rather than the admin role the table names.
- **Not implemented:** Audit export and a cluster-level audit listing; session-id rotation; a general API rate limit (login and token endpoints are limited).
- **Not implemented:** etcd encryption at rest by the installer (managed clusters decide this) and the backup-exclusion label on config var Secrets.
- **Superseded:** Cross-project access "through a declared binding": bindings are project-local; projects never reach each other.
