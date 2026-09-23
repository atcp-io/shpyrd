# RFC-0034 Domains and certificates

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0011

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

Several base domains per cluster, project hostnames as a subdomain of any of them or as a
fully custom domain the owner points at the cluster, ownership verification for custom
domains, Let's Encrypt certificates on cloud profiles (the development CA locally), and a
Domains card that says what is missing (DNS, certificate) and how to fix it.

## Motivation

Today a project gets `<slug>.<the one cluster domain>` plus raw `domains` entries with
certificates from the cluster issuer only, which browsers do not trust outside the
laptop.

### Goals

- `shpyrd domains add shop.acme.com` → instructions → verified → certificate → serving.
- `shpyrd domains add shop.apps.example.com` on a second wildcard the cluster owns.

### Non-Goals

- Managing DNS records for you (RFC-0036) and internal/external exposure (RFC-0036).

## Proposal

- `SHPYRD_DOMAINS` (comma separated) replaces the single domain for projects; the first is
  the default. Platform hostnames stay on the first.
- `Domain` entries live on the App (`spec.domains: [{host, verified}]`); the controller
  renders Ingress rules and one Certificate per host (or uses a wildcard Certificate per base
  domain when RFC-0036 provides DNS-01).
- **Custom domains** need proof: a TXT record `_shpyrd-verify.<host>` with a token shown by
  `shpyrd domains add` and the card; the controller checks it (and the CNAME/A pointing at
  the cluster) before serving. Platform admins may skip verification.
- **Issuers**: profile var `SHPYRD_CLUSTER_ISSUER` (local: `shpyrd-ca`; cloud: `letsencrypt`
  with `SHPYRD_ACME_EMAIL`, HTTP-01 through ingress-nginx; staging issuer for tests).
- Status per domain: `dns: ok|missing`, `verification: ok|pending`, `certificate:
  ready|issuing|failed (reason)`; the card shows the exact records to create.
- `shpyrd domains add|remove|list|verify`.

## Design Details

- Ingress-nginx refuses duplicate hosts across namespaces; the controller checks first and
  reports "host already used by project X".
- HTTP-01 needs port 80 reachable; documented per profile.

## Open questions

1. Verification required for custom domains (TXT record) with an admin bypass? Default:
   yes.

## Implementation History

- 2026-09-22: RFC written.
