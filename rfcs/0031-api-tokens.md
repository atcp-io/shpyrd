# RFC-0031 Per-user API tokens

**Status:** implemented

**Owner:** Patrick Negri

**Depends on:** RFC-0008 (implemented), RFC-0033 phase 1 (control-plane database)

**Creation date:** 2026-09-22

**Last update:** 2026-09-26

## Summary

Personal and project API tokens with roles and expiry: `shpyrd tokens create --project shop
--role developer --expires 90d`, stored hashed, listed and revoked in the dashboard,
accepted by the same middleware as the admin token. The right credential for CI and for
the remote MCP connector (RFC-0032); the admin token stays for bootstrap.

## Motivation

Automation gets the shared, unattributable, all-powerful admin token today.

### Goals

- Tokens carry an identity (the creating user or a named service), roles no wider than the
  creator's, an expiry and a last-used time.
- Revocation is immediate.

### Non-Goals

- OAuth client credentials or token exchange.

## Proposal

- Tokens live in the control-plane database (RFC-0033 phase 1), table `api_tokens`:
  name, owner (email), roles (a platform role or project roles), `expiresAt`,
  `lastUsedAt`. The secret value `shp_<id>_<random>` is shown once; only the SHA-256 of the
  random part is stored. Names are unique per owner.
- Middleware: `Authorization: Bearer shp_...` (or `X-Shpyrd-Token`) → look up by hash,
  check expiry → identity `{subject: "token:<id>", email: owner, name: token name,
  provider: "api-token"}` with the token's roles **intersected with the owner's current
  roles at use time**: a demoted owner cannot keep power through a token, a suspended
  owner's tokens stop with them, and a token only works at the workspace it was created in.
  The owner is resolved as the person the workspace knows (email, provider, last groups
  claim), so roles that come through IdP groups or the built-in `everyone` team count.
- Creation is clamped: a token never carries a role above what its creator holds at that
  moment, and a token cannot create tokens (a leaked CI credential must not grant itself
  persistence). People and the admin token create; anyone may revoke their own, platform
  admins may revoke any.
- CLI `shpyrd tokens create|list|revoke` (over the API, after `shpyrd login`); dashboard
  Workspace → **API tokens** tab (the form only offers the roles the caller has);
  `shpyrd login --token shp_...` for CI and laptops; audit `token.create|revoke`, and
  audit entries made with a token read `owner (token name)`.

## Design Details

- `lastUsedAt` is written at most once a minute per token.
- Expiry is optional (`expiresIn`: `30d`, `90d`, `365d`, …); the dashboard defaults to 90
  days, the CLI to none.
- The admin token (RFC-0008) is unchanged and remains break-glass; tokens created with it
  have no owner and carry exactly the roles they were given.

## Implementation status

Implemented in v0.9.0. Known gaps:

- The `ApiToken` CRD of the first draft was replaced by a database table; nothing to
  migrate since no release shipped the CRD.
- `shpyrd tokens create` from the CLI works when signed in as a person or with the admin
  token; a session opened with a `shp_` token cannot mint further tokens (by design). Until
  the CLI's browser sign-in lands (RFC-0052), people without cluster access create their
  tokens in the dashboard.
- No per-token IP allow list or usage counters; RFC-0025 (durable audit) is where usage
  history belongs.

## Implementation History

- 2026-09-22: RFC written.
- 2026-09-26: implemented (v0.9.0): `api_tokens` table, middleware, clamping and
  intersection with the owner's live roles, CLI `tokens` commands, Workspace tab.
