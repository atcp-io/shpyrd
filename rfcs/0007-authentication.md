# RFC-0007 Authentication

**Status:** implemented (3.1 local users); 3.2/3.3 pending

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Replace the single admin token with real identities in three steps that share one
implementation: the shpyrd server becomes an **OIDC relying party**; local users come
from a bundled **Dex** issuer (`auth-local`); a full email/password account system comes
from a fuller issuer or from lifecycle flows built on Dex (`auth-email`); Okta (or any
OIDC provider) is configuration (`auth-oidc`). The admin token stays for bootstrap and
automation.

## Motivation

A token shared by everyone is fine for one developer on a laptop and wrong for a team:
no names in the audit trail, no way to revoke one person, no roles (RFC-0008).

### Goals

- Users log into the dashboard with their own credentials; the CLI can act as a user.
- Local accounts without any external service; email/password with invites and reset when
  wanted; Okta/GitHub/Google when the company has them.
- One relying-party implementation for all of the above; providers can be swapped or
  combined per cluster.

### Non-Goals

- Being an identity provider for other systems.
- SAML (OIDC only; every provider that matters speaks it).

## Proposal

### Why an OIDC issuer instead of our own users table

Rolling our own users table (bcrypt, sessions, reset, MFA) is fine for five users and a
liability afterwards. Argo CD and Weave GitOps bundle **Dex**; shpyrd does the same and only
implements OIDC (authorization code + PKCE, cookie session, refresh, logout).

### What Dex does and does not do

Dex is an OIDC issuer with pluggable backends. It has a **local password connector**: users
(email, bcrypt hash, name) live in its database, it renders the email/password login page
during the OIDC flow, and users are managed through its gRPC API (`CreatePassword`,
`UpdatePassword`). It does **not** send emails, offer self-service password reset,
invitations, verification, MFA or lockout: those are account lifecycle features Dex leaves
out by design.

### Steps

| Step | Extension | What users get | How |
| --- | --- | --- | --- |
| **3.1 Local users** | `auth-local` | dashboard login with email/password; admins manage accounts (`shpyrd users add|passwd|rm`, dashboard Users page) | Dex + local connector; users stored by Dex; the admin token remains for automation |
| **3.2 Email/password system** | `auth-email` | invitations, email verification, self-service reset, optional TOTP | either (a) small flows built by shpyrd on Dex (SMTP extension sends signed one-time links, the callback calls Dex's API; TOTP handled by the shpyrd login step) or (b) **Zitadel** or **Keycloak** as the issuer, which ship all flows; same relying-party code, per-cluster choice. Default recommendation: (b) Zitadel for teams wanting self-service, Dex for small or air-gapped setups |
| **3.3 Okta / OIDC** | `auth-oidc` | company login, groups → teams | issuer URL, client id/secret; Dex can federate several providers behind one login page when local and corporate accounts must coexist |

### Sessions and the CLI

- Dashboard: HttpOnly, Secure, `SameSite=Lax` session cookie holding an opaque id; the
  server keeps the OIDC tokens; CSRF token for cookie-authenticated mutations; idle and
  absolute timeouts; logout revokes.
- CLI: `shpyrd login` runs the OIDC device flow (or opens the browser) and stores a token in
  `~/.shpyrd/config`; used for API calls that need a user (audit, dashboard parity).
  Cluster operations keep using the kubeconfig; the same issuer can be configured on the
  Kubernetes API server so `kubectl` users are the same identities.
- Automation keeps the admin token (rotated with `shpyrd cluster token --rotate`); later,
  per-user API tokens.

## Design Details

- `AuthProvider` interface (RFC-0002): `Begin`, `Complete`, `Refresh`, `Logout`; the OIDC
  implementation covers every provider; `auth-local`'s extra is the Dex component and the
  `users` management commands/API.
- Identity: `{subject, email, name, groups}` normalized from claims; stored session
  records include provider and expiry for the audit log (RFC-0008).
- Dex component: `dexidp/dex` chart, Postgres-free (kubernetes CRD storage) for local; its
  own certificate from the cluster issuer; internal URL for the server, external at
  `https://auth.<domain>`.
- Token endpoints and callbacks are rate limited; login failures are audited.

### Drawbacks

- One more component (Dex) for local users. Its footprint is small and the alternative is
  owning password security ourselves.

## Implementation History

- 2026-09-22: RFC written; phase C implements the relying party and `auth-local`.
- 2026-09-22: Implemented step 3.1. Server: OIDC relying party (go-oidc; authorization code
  with PKCE and nonce, discovery reached through the ingress controller service with the
  cluster CA so issuers on the cluster domain resolve inside the cluster), sessions in
  memory mirrored into Secret `shpyrd-sessions` (identity only, 12h idle / 7d absolute),
  `shpyrd_session` HttpOnly cookie plus `shpyrd_csrf` double-submit cookie checked on
  mutations, `/api/auth/{providers,login,callback,logout}` and `/api/me`, identity on the
  request context for extensions and the coming audit log; the admin token stays.
  `auth-local`: Dex component (kubernetes CRD storage, `enablePasswordDB`, static client
  from the hook-generated `shpyrd-oidc-client` Secret, `https://auth.<domain>` behind the
  ingress with a cluster-issuer certificate); accounts are Dex `Password` objects managed
  by the server (`/api/users`) and `shpyrd users add|list|passwd|rm` (bcrypt, Dex's object
  naming), so no Dex API client is needed and accounts survive disable/enable. Dashboard:
  provider buttons on the login page, session gate, user menu with sign-out, Users page.
  Not done: `shpyrd login` for the CLI (kubeconfig remains its identity), rate limiting of
  the callback, and steps 3.2/3.3.
