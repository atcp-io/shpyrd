# RFC-0058 External identity providers (Okta and OIDC, GitHub, Google)

**Status:** implementable

**Owner:** unassigned

**Depends on:** RFC-0012 (sign-in page), RFC-0007 (implemented)

**Creation date:** 2026-09-23

**Last update:** 2026-09-23

## Summary

Company and public identity providers on the sign-in page: any OpenID Connect issuer
(Okta first) connected directly to shpyrd's relying party through a new `auth-oidc`
extension, and GitHub and Google through Dex connectors. Users arrive with their email and
their groups, so an Okta group or a GitHub team can be a shpyrd Team. Split out of
RFC-0012, which defines the page these providers appear on.

## Motivation

A team adopting shpyrd already has an identity provider. Today the relying party
(RFC-0007) can talk to any issuer but has no configuration surface: the only issuer is the
Dex that `auth-local` installs. Sign-in with the company account is the second thing an
evaluator asks for, right after the product looking like itself (RFC-0012).

### Goals

- `shpyrd auth oidc set ...` connects Okta or any OIDC issuer in one command; the provider
  appears as a button on the sign-in page; groups flow into Teams.
- `shpyrd auth connector add github|google ...` does the same through Dex.
- Several providers coexist (local accounts and the company IdP during a migration).
- Tested against a real Okta developer tenant and a real GitHub OAuth app before the RFC
  is marked implemented.

### Non-Goals

- SAML (Okta and Entra both speak OIDC; Dex has a SAML connector if ever needed).
- Just-in-time provisioning of Teams from groups: groups are matched against the Team's
  `groups` list (RFC-0008), Teams are still created by an admin.
- SCIM, account deprovisioning (a user removed at the IdP simply cannot sign in again;
  their session expires within the session lifetime).

## Proposal

- **`auth-oidc` extension.** `shpyrd auth oidc set --id okta --label "Okta" --issuer
  https://acme.okta.com --client-id ... --client-secret ...` stores the secret in Secret
  `shpyrd-oidc-<id>` and the rest as install vars (`SHPYRD_OIDC_<ID>_ISSUER`, `_LABEL`,
  `_CLIENT_ID`, `_SCOPES`); the server registers an `ext.OIDCProvider` per configured id at
  startup. `shpyrd auth oidc list|remove <id>`. The login page lists providers in
  registration order; `--label` is the button text. Scopes default to
  `openid email profile groups`; `--scopes` overrides for issuers that name the groups
  scope differently.
- **GitHub and Google through Dex.** `shpyrd auth connector add github --client-id ...
  --client-secret ... [--org acme]` and `... add google ...` render a connector into Dex's
  config (`auth-local` must be enabled; the command says so otherwise) and restart Dex.
  Dex shows its connector chooser only when more than one connector is configured; with
  the local connector present that is always the case, so the connector is linked directly
  from our page with `connector_id` in the authorization request, skipping Dex's page.
  GitHub teams become Dex groups (`org:team` form; `--org` limits sign-in to members);
  Google groups need a service account and are configuration only.
- **Identity mapping.** Email from the `email` claim (verified when the provider says so;
  GitHub needs a verified primary email), display name from `name`, groups from `groups`.
  A user signing in through two providers with the same email is the same shpyrd user
  (RFC-0008 keys roles by email); the audit trail records the provider used.
- **Sign-out.** RP-initiated logout from RFC-0012 covers direct providers that publish
  `end_session_endpoint`; connector sign-ins end at Dex.

### Alternatives

- Everything through Dex (Okta as a Dex connector too): one code path, but every corporate
  sign-in would bounce through Dex, and clusters without `auth-local` would need Dex only
  for that; rejected in favour of direct OIDC for issuers and Dex for the providers that
  are not OIDC-shaped (GitHub) or that benefit from Dex's connector code (Google).
- Configuration by YAML only: rejected, the CLI is how everything else is configured.

## Design Details

- Redirect URI to register at the provider: `${SHPYRD_DASHBOARD_URL}/api/auth/callback`
  (one for all direct providers; the `state` carries the provider id, as the relying party
  already does for Dex). Docs list the exact steps for Okta (app integration of type "OIDC
  Web Application", groups claim `groups` with filter "Matches regex .*" on the default
  authorization server) and GitHub (OAuth App, callback URL, `read:org` for teams).
- Okta groups: without a `groups` claim configured on the authorization server Okta sends
  none; the docs say so and `shpyrd auth oidc check <id>` performs a discovery fetch and
  reports the claims the issuer advertises.
- `pkg/ext/authoidc`: extension registering providers from install vars + Secrets;
  `Routes` adds nothing (the relying party is generic); `CLI` adds `shpyrd auth oidc`.
- `pkg/ext/authlocal`: `shpyrd auth connector` renders connectors into the Dex ConfigMap
  from a template, restarts the Deployment, and lists them.
- Login page: buttons from `GET /api/auth/providers`, which gains `label` and `kind`
  (`oidc`, `dex-connector`) so the page can render an icon for GitHub/Google.
- Tests: the fake issuer covers direct providers (a second issuer with groups); connector
  rendering is unit-tested against Dex's config schema; the Okta and GitHub runs are
  manual with a checklist in the RFC's Implementation History.
- Enable/disable: providers exist only when configured; `remove` deletes the Secret and
  the vars and the button disappears on the next server start. Removing a provider does
  not remove roles: they are keyed by email.

## Settled questions

1. Providers on day one: direct OIDC verified against Okta, GitHub verified through Dex;
   Google and Microsoft Entra documented as configuration only.
2. Provider ids are fixed once created (they appear in the audit trail and in `state`);
   labels can change.

## Implementation History

- 2026-09-23: RFC written, split out of RFC-0012.
