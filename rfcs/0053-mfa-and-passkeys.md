# RFC-0053 Multi-factor authentication and passkeys

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0012, RFC-0014

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Second factors for local accounts: time-based one-time passwords (TOTP) and **passkeys**
(WebAuthn), enrolled from the user menu, asked for after the password, with recovery
codes; passkeys also work as passwordless sign-in. A cluster setting requires a second
factor for platform admins or for everyone.

## Motivation

Password-only accounts administer production clusters.

### Goals

- Enrol in a minute; sign-in asks for the code; recovery codes work once.
- `SHPYRD_REQUIRE_MFA=platform-admins|all|off`.

### Non-Goals

- MFA for external providers (theirs).

## Proposal

- After the password step (RFC-0012) succeeds for an enrolled user, the session is
  created in a `pendingMFA` state that only allows `/api/auth/mfa/*`; a correct TOTP
  (RFC 6238, 30s, one step of drift) or a WebAuthn assertion completes it. TOTP secrets,
  passkey credentials (public key, counter, name) and hashed recovery codes live in Secret
  `shpyrd-mfa-<user>`.
- Passkeys: the server is a WebAuthn relying party (RP id = dashboard host); enrolment and
  assertion through the browser's `navigator.credentials`; a passkey registered as "sign in
  with passkey" allows passwordless sign-in (user verification required), still subject to
  the same session rules.
- User menu: Security section (add TOTP, add passkey, recovery codes, remove); Users page
  shows enrolment status; admins can reset factors for a user (audited).

## Implementation History

- 2026-09-22: RFC written.
