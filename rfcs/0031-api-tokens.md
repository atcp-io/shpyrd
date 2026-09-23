# RFC-0031 Per-user API tokens

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0008 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

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

- `ApiToken` objects in `shpyrd-system`: `spec.name`, `spec.owner` (email), `spec.roles`
  (platform role and/or project roles), `spec.expiresAt`; `status.lastUsedAt`. The secret
  value `shp_<id>_<random>` is shown once; SHA-256 stored.
- Middleware: `Authorization: Bearer shp_...` → look up by id, constant-time compare, check
  expiry → identity `{subject: token id, email: owner, provider: "token:<name>"}` with the
  token's roles (intersected with the owner's current roles at use time, so a demoted owner
  cannot keep power through a token).
- CLI `shpyrd tokens create|list|revoke`; Users page → "API tokens" section per user;
  audit `token.create|revoke`, and audit entries made with a token name the token.
- `shpyrd deploy --token` / `SHPYRD_TOKEN` for CI uploads.

## Design Details

- Lookups cached briefly; `lastUsedAt` written at most once a minute.

## Implementation History

- 2026-09-22: RFC written.
