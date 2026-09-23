# RFC-0036 DNS providers and load balancer exposure

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0034, RFC-0035

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

DNS records created automatically for platform and project hostnames (ExternalDNS with
Route53 and Cloudflare), wildcard certificates through DNS-01 with the same providers, and
two ingress classes on cloud profiles: an external load balancer for public apps and an
internal one for the dashboard, Grafana, the issuer and any project marked internal.

## Motivation

Production platforms live behind company networks; the dashboard must not be public by
default, while apps choose.

### Goals

- `exposure: internal|external` per project; platform services internal by default on
  cloud profiles.
- Hostnames resolve without manual records.

### Non-Goals

- Caddy as an ingress (no mature Kubernetes integration; cert-manager and the development CA
  already cover automatic HTTPS locally).

## Proposal

- Extensions `dns-route53` and `dns-cloudflare`: ExternalDNS watching Ingresses, provider
  credentials via IRSA (AWS) or an API token Secret (Cloudflare); cert-manager DNS-01
  solvers for wildcard certificates per base domain.
- Ingress classes `shpyrd-external` and `shpyrd-internal` (two ingress-nginx controllers; the
  internal one with `service.beta.kubernetes.io/aws-load-balancer-scheme: internal`); the
  local profile maps both to the single controller.
- `SHPYRD_PLATFORM_EXPOSURE=internal|external` for dashboard/Grafana/auth; App
  `spec.exposure` (default `external`, `shpyrd projects exposure shop internal`).
- Status: the Domains card shows which load balancer serves a host and its address.

## Open questions

1. Providers first: Route53 and Cloudflare? Default: yes.
2. Defaults on cloud: platform internal, projects external? Default: yes.

## Implementation History

- 2026-09-22: RFC written.
