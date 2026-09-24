# RFC-0034 Custom domains

**Status:** in progress

**Owner:** Patrick Negri

**Depends on:** RFC-0036 (front doors, implemented), RFC-0061 (wildcard certificate,
implemented for OCI DNS)

**Creation date:** 2026-09-22

**Last update:** 2026-09-24 (rewritten around the platform pattern; scope narrowed to
custom domains)

## Summary

A project is always served at `<slug>.<cluster domain>`. Its owner can add domains they
control — `www.myprod.com`, `myprod.com` — and point them at that hostname with a CNAME
(or at the front door's address with an A record when the domain is a zone apex). Once DNS
resolves here, a certificate is issued for the domain with no further action: pointing DNS
at the platform is the proof of control, as on Heroku, Render and Railway. `shpyrd domains
add|list|rm`, `POST/GET/DELETE /api/projects/:slug/domains`, and a Domains card that shows
the exact record to create and the state of each host.

## Motivation

Before this RFC `spec.domains` replaced the project's hostname and every host shared one
certificate, so a domain whose DNS was not ready blocked the project's own certificate,
and nothing told the owner what record to create or whether it had propagated.

### Goals

- `shpyrd domains add www.myprod.com` prints the one record to create and waits until the
  domain serves; the card shows DNS and certificate per host and refreshes on its own.
- A pending or misconfigured custom domain never affects the project's own hostname.
- A hostname is served by one project only; the second one to ask is told who has it.

### Non-Goals

- Wildcard custom domains (`*.myprod.com`): Let's Encrypt only issues those through
  DNS-01 in the owner's zone, which shpyrd does not control.
- Several cluster domains (`SHPYRD_DOMAINS`): a later RFC; every project keeps one
  platform hostname.
- Ownership verification records (`TXT _shpyrd-verify`): the platforms that grew largest
  do without; a domain pointing at the front door is under its owner's control, and
  uniqueness across projects prevents squatting inside the platform.

## Proposal

- `spec.domains` becomes additive: the default hostname is always served first, custom
  domains after it, normalised (lower case, no trailing dot) and deduplicated.
- One cert-manager `Certificate` per host that needs one (`<slug>-<host>-tls`), owned by
  the App: custom domains always; the default host only when no platform wildcard serves
  it (RFC-0061). The Ingress declares one TLS entry per host and carries no cert-manager
  annotation; the legacy ingress-shim certificate is removed once.
- The controller resolves each custom domain and compares with the front door: a CNAME
  to the project's hostname or an A record to the front door's address is `ok`; no record
  is `missing`; another target is `wrong`. It reads the certificate's conditions
  (`issuing`, `ready`, `failed` with cert-manager's reason) and reports both in
  `status.domains`, polling every 30 s while anything is pending.
- Adding a domain validates the hostname, refuses wildcards, the platform's own hosts and
  the project's hostname, and refuses a host already served by another project (naming
  it). Removing a domain removes its certificate.
- Projects with `exposure: internal` (RFC-0036) get the internal load balancer as the A
  target; DNS-01 is not needed since the wildcard covers the platform hostname and HTTP-01
  reaches the front door the domain points at.

### User Stories

- "Serve the shop at www.myprod.com": `shpyrd domains add www.myprod.com --project shop`,
  create `CNAME www.myprod.com -> shop.oci.shpyrd.io`, done a few minutes later.
- "Serve it at myprod.com": the same with `A myprod.com -> 147.15.59.84` (the reserved
  address of RFC-0035's Terraform), or ALIAS/ANAME where the provider has it.

### Alternatives

- **Per-domain unique CNAME targets** (Heroku's `<haiku>.herokudns.com`, Railway's
  `<random>.up.railway.app`). Isolates routing per domain; unnecessary here since
  ingress-nginx routes by the `Host` header and the platform hostname is already unique
  per project.
- **A Verify button** (Render). The controller polls instead; the card refreshes itself.
- **TXT ownership record** (Railway). Adds a step for every domain to defend against a
  case uniqueness already covers.

## Design Details

- `App.spec.domains []string`; `App.status.domains []DomainStatus{host, dns, target,
  address, certificate, message}`.
- Controller (`internal/controller/domains.go`): `domains()`, `ingressTLS()`,
  `reconcileCertificates()`, `domainStatuses()` with a pluggable resolver; the external
  address is read from the ingress-nginx Service at start and on demand.
- API (`pkg/api/domains.go`): hostname validation, conflict check over every Ingress,
  `DomainsResult` with the CNAME target, the A address and per-host state.
- CLI (`internal/cli/domains.go`): `add` prints the record and waits (30 minutes,
  Ctrl-C keeps the domain), `list` is the table, `rm`.
- Dashboard: `DomainsCard` on the project overview, polling every 10 s while pending.
- Let's Encrypt limits: 50 certificates per registered domain per week, 5 failed
  validations per hostname per hour; cert-manager backs off on its own.

## Implementation History

- 2026-09-22: RFC written (several base domains, TXT verification).
- 2026-09-24: Rewritten after reading how Heroku, Fly.io, Render and Railway do it:
  additive custom domains on top of the platform hostname, CNAME/A to it as the proof of
  control, per-host certificates, no TXT record. Implemented in the same pass; verified on
  the OKE proof of concept with `myprod.shpyrd.io`.
