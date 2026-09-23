# RFC-0012 Sign-in experience: shpyrd's own sign-in page

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0007 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-23

## Summary

One sign-in page rendered by shpyrd: email and password for local accounts without ever
showing Dex's page, clear errors in place, sign-out that also ends the session at the
issuer. External identity providers (Okta and any OpenID Connect issuer, GitHub, Google)
were part of this RFC and are now RFC-0058, which builds on the page defined here.

This RFC and RFC-0058 together cover RFC-0007 step 3.3 (`auth-oidc`) and the "customize
Dex" and "sign-in screen" requests.

## Motivation

Today local sign-in redirects to Dex's default page ("Log in to Your Account", Dex logo),
which looks like a different product. It is the first screen anyone sees on a cluster with
`auth-local` enabled, and the one screenshot of the tour that does not look like shpyrd.

### Goals

- Users never leave the shpyrd look: email/password on our page, errors shown in place,
  the same page ready to list provider buttons (RFC-0058).
- Existing sessions, CSRF protection, one-time tickets (`shpyrd cluster dashboard`) and the
  admin token keep working unchanged.
- Signing out of shpyrd signs the user out of the issuer too.

### Non-Goals

- External providers and their configuration (RFC-0058).
- Invitations, password reset, email verification, lockout (RFC-0014); MFA (RFC-0053).
- A sign-in for the CLI (RFC-0052).

## Proposal

- **Local accounts without Dex's UI.** Dex gets `oauth2.passwordConnector: local`, which
  enables the resource-owner password grant for the `local` connector. A new endpoint
  `POST /api/auth/password {email, password}` exchanges the credentials for an `id_token`
  at Dex's token endpoint (server to server, client `shpyrd` with its secret), verifies the
  token exactly like the authorization-code callback does today and opens the same session
  (cookie `shpyrd_session`, CSRF token, mirrored Secret). Wrong credentials return 401 with
  a message the page shows in place; the endpoint uses the existing login rate limiter and
  every attempt is audited (success and failure, by email).
- **The page.** `/login` renders the email/password form when `auth-local` is enabled, a
  list of provider buttons (empty until RFC-0058 registers some), "Use the admin token" only
  when the token is enabled, and `login_error`/`next` handling as today. Copy: "Sign in to
  shpyrd", the cluster's name or domain under it.
- **Dex's fallback page.** Dex still serves its page in edge cases (a provider button from
  RFC-0058 that goes through a Dex connector when several are configured). It gets a
  minimal theme from a ConfigMap (`frontend.theme` with logo, orange accent, "Sign in to
  shpyrd") so even the fallback does not look foreign.
- **Sign-out.** `POST /api/auth/logout` clears the session as today and, when the issuer
  publishes `end_session_endpoint` in its discovery document, redirects the browser there
  with `id_token_hint` and `post_logout_redirect_uri` set to the dashboard. Dex publishes
  the endpoint; direct providers from RFC-0058 use the same code.

### Alternatives

- Theming Dex only (no password endpoint): keeps the redirect and a second page; rejected,
  the page is the product's front door.
- Replacing Dex with our own password verification: loses the connectors RFC-0058 needs
  and the federation Dex gives for free; rejected.

## Design Details

- Dex config additions: `oauth2.passwordConnector: local`. The `shpyrd` client keeps its
  `secretEnv`; the server already has the secret for the code flow.
- `pkg/api/auth.go`: `authPassword` handler; token exchange through the existing
  `oidc.Provider` (`golang.org/x/oauth2` `PasswordCredentialsToken`), then the same
  `verifyAndOpenSession(idToken, next)` path as `authCallback`.
- Sessions: unchanged (`pkg/api/sessions.go`); the id_token is kept in the session record so
  logout can send `id_token_hint`.
- UI: `ui/src/pages/login.tsx` gains the form; `api.ts` gains `passwordLogin`. The provider
  list comes from `GET /api/auth/providers` as today; `auth-local` is not listed as a
  button any more once the form exists.
- Tests: the fake issuer in `pkg/api/auth_test.go` gains a token endpoint accepting
  `grant_type=password` (and rejecting a wrong password) and an `end_session_endpoint`;
  tests cover the session opened by password, the rate limit, the audit entries and the
  logout redirect.
- Enable/disable: the form appears with `auth-local`; without the extension the page shows
  the providers and the admin token as today. Nothing changes for clusters without
  `auth-local`.

## Settled questions

1. "Remember me" (30-day absolute session instead of 7)? No; sessions stay as they are.
2. Providers on day one: moved to RFC-0058.

## Implementation History

- 2026-09-22: RFC written (own page, local sign-in and external providers).
- 2026-09-23: external providers split into RFC-0058 so this part can ship and be tested on
  a local cluster alone; status `implementable`.
