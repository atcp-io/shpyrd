# RFC-0013 Email delivery

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0002 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

A `mail` extension that lets the platform send email (SMTP, optionally an HTTP provider),
with a test command. Used by account lifecycle flows (RFC-0014) and notifications
(RFC-0030).

## Motivation

Invitations, password resets and alerts need a sender; every consumer configuring its own
SMTP would be repetitive and error prone.

### Goals

- One configuration, one sender identity, one test: `shpyrd mail test you@example.com`.
- Credentials never printed; delivery failures visible in the audit trail.

### Non-Goals

- Receiving mail. Templating beyond simple text/HTML with a header and footer.

## Proposal

- `shpyrd extensions enable mail`, then `shpyrd mail set --host smtp.example.com --port 587
  --user ... --password ... --from "shpyrd <noreply@example.com>" [--starttls|--tls]`; the
  password in Secret `shpyrd-mail`, the rest as vars.
- `pkg/ext/mail` exposes `Sender` (`Send(ctx, Message{To, Subject, Text, HTML})`) through
  `ext.Deps` for other extensions; rate limited per recipient.
- Dashboard: Cluster page card "Email" with status (configured, last test result).

## Design Details

- Go `net/smtp` with STARTTLS/implicit TLS; optional HTTP provider adapters behind the same
  `Sender` interface.
- Templates: text plus a small HTML wrapper with the wordmark; links use
  `SHPYRD_DASHBOARD_URL`.

## Open questions

1. SMTP only, or also an HTTP provider (SES API, Resend, Postmark)? Default: SMTP first,
   the adapter interface ready for HTTP providers.

## Implementation History

- 2026-09-22: RFC written.
