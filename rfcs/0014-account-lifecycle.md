# RFC-0014 Account lifecycle

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0012, RFC-0013

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Self-service for local accounts (RFC-0007 step 3.2): invitations, email verification,
password reset, lockout after repeated failures. Built on Dex's local accounts with flows
owned by shpyrd; Dex itself offers none of these.

## Motivation

Admins set passwords by hand today and pass them along. Teams need "invite by email" and
"I forgot my password".

### Goals

- `shpyrd users invite ada@example.com` / dashboard "Invite" sends a link; the person sets
  their own password.
- "Forgot password" on the login page.
- Verified emails; lockout with a clear message and a timed release.

### Non-Goals

- MFA/TOTP (a follow-up RFC once this lands).
- Accounts from external providers (their lifecycle belongs to the provider).

## Proposal

- **Signed links**: the server issues single-use tokens (random, hashed in a short-lived
  Secret like login tickets, 24h for invites, 1h for resets) and serves two small pages:
  `/account/set-password?token=...` (invite) and `/account/reset?token=...`. Completing the
  form creates or updates the Dex Password object (`pkg/ext/authlocal.Store`) and marks
  `emailVerified: true`.
- **Invite**: `POST /api/users/invite {email, name}` (platform admins) or a Users page
  action; the account exists in a "pending" state (a Password object with an unusable hash
  and an `invited` annotation) until the link is used; re-invite regenerates the link.
- **Reset**: public `POST /api/auth/reset {email}` (always answers "if the account exists,
  an email was sent"; rate limited); the link sets a new password and ends existing
  sessions of that user.
- **Lockout**: the password endpoint (RFC-0012) counts failures per account; after 10 in
  15 minutes the account is locked for 15 minutes (login says so); audited.
- Users page shows verified/pending/locked.

## Design Details

- Tokens: 32 random bytes, SHA-256 stored, label `shpyrd.io/account-token`, expiry field.
- Email templates: invite, reset, "your password was changed".
- Audit actions: `user.invite`, `user.reset_requested`, `user.reset`, `user.locked`.

## Open questions

1. MFA in this RFC or the follow-up? Default: follow-up (keeps this at ~2 days).

## Implementation History

- 2026-09-22: RFC written.
