# RFC-0012 Sign-in experience

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0007 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

One branded sign-in page rendered by shpyrd: email and password for local accounts
without ever showing Dex's page, buttons for external providers (Okta and any OpenID
Connect issuer, GitHub and Google through Dex connectors), clear errors. Covers RFC-0007
step 3.3 (`auth-oidc`) and the "customize Dex" and "sign-in screen" requests.

## Motivation

Today local sign-in redirects to Dex's default page ("Log in to Your Account", Dex logo),
which looks like a different product. Company identity providers are supported by the
relying party but have no configuration surface.

### Goals

- Users never leave the shpyrd look: email/password on our page, provider buttons next to
  it, errors shown in place.
- `shpyrd auth oidc set ...` connects Okta or any OIDC issuer; GitHub and Google available.
- Existing sessions, CSRF, tickets and the admin token keep working.

### Non-Goals

- Invitations, password reset, MFA (RFC-0014).

## Proposal

- **Local accounts without Dex's UI**: enable Dex's `passwordConnector: local` and the
  OAuth2 password grant for the `shpyrd` client; `POST /api/auth/password {email, password}`
  exchanges credentials for an id_token at Dex's token endpoint (server to server), verifies
  it like the code flow and opens the same session. Rate limited (existing limiter) and
  audited. Dex's page remains only for connector selection edge cases and gets a minimal
  theme (logo, colours, "Sign in to shpyrd") through `frontend.theme`/`frontend.dir` from a
  ConfigMap.
- **External providers**: `auth-oidc` extension: `shpyrd auth oidc set --id okta --label Okta
  --issuer https://acme.okta.com --client-id ... --client-secret ...` (secret in
  `shpyrd-oidc-<id>`, the rest as install vars) registers an `ext.OIDCProvider` at startup;
  several providers coexist (the login page lists them in registration order). GitHub and
  Google through Dex connectors: `shpyrd auth connector add github --client-id ...` renders a
  connector into Dex's config; their users appear with `email` and `groups` when the
  provider gives them (GitHub teams → Dex groups → shpyrd Teams).
- **Login page**: email/password form (when `auth-local` is on), provider buttons, "Use the
  admin token" only when the token is enabled, `login_error` and lockout messages in place,
  "Remember me" (optional, see questions), a link to "Forgot password" once RFC-0014 lands.
- RP-initiated logout for providers that support `end_session_endpoint`.

## Design Details

- Dex config additions: `oauth2.passwordConnector: local`, the client keeps `secretEnv`.
- Redirect URI for external providers: `${SHPYRD_DASHBOARD_URL}/api/auth/callback`; the
  docs list what to register at Okta/GitHub/Google.
- Groups: Okta requires a `groups` claim on the authorization server; documented.
- Tests: fake issuer (exists) extended with a token endpoint accepting `grant_type=password`.

## Open questions

1. Providers that must work on day one? Default: direct OIDC (tested against Okta) and
   GitHub via Dex; Google and Microsoft Entra as configuration only.
2. "Remember me" (30-day absolute session instead of 7)? Default: no.

## Implementation History

- 2026-09-22: RFC written.
