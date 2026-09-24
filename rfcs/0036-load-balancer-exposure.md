# RFC-0036 Load balancer exposure: internal and external front doors

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0035 (cloud profiles); RFC-0061 (DNS providers) for public certificates
on internal hosts

**Creation date:** 2026-09-22

**Last update:** 2026-09-24 (rewritten: DNS providers moved to RFC-0061; OKE specifics)

## Summary

Cloud profiles get two front doors: an **external** load balancer with a public address
for the applications people publish, and an **internal** one, reachable only from the
company network (VPN, peered VCN, bastion), for the platform itself — dashboard, sign-in,
Grafana — and for any project marked internal. The platform is internal by default on cloud
profiles; each project chooses with one setting. The local profile keeps its single front
door and maps both names to it.

## Motivation

The OKE proof of concept publishes the dashboard and the sign-in page on the internet
because there was one load balancer. Production platforms do not: the control plane of a
company's applications belongs behind the company network, while the applications choose
per case (a public storefront, an internal back office). Both should be a setting, not an
infrastructure project.

### Goals

- `exposure: internal | external` per project (`shpyrd.yaml`, CLI, dashboard toggle);
  platform endpoints internal by default on cloud, `SHPYRD_PLATFORM_EXPOSURE=external` for
  proofs of concept.
- Switching exposure is a release-free operation that moves the hostnames and keeps the
  certificates valid.
- The cluster page shows which front door serves what and its addresses.

### Non-Goals

- DNS record automation and wildcard certificates (RFC-0061).
- How users reach the internal network (VPN, bastion, peering): documented per provider,
  not provisioned.
- Custom domains on projects (RFC-0034); this RFC covers `<name>.<domain>` hostnames.
- Cloud profiles other than `oci` in this RFC's history; the AWS mapping is spelled out
  for RFC-0035's AWS work.

## Proposal

- Two ingress-nginx controllers on cloud profiles, ingress classes `shpyrd-external` and
  `shpyrd-internal`, each with its own Service of type LoadBalancer: the external one as
  today, the internal one annotated for a private load balancer on the profile's private
  load balancer subnet. The local profile creates both IngressClass objects pointing at
  its single controller, so manifests are identical everywhere.
- The App controller renders each project's Ingress with the class for its exposure
  (default `external`); the shpyrd, Dex and Grafana Ingresses use the platform's
  (`SHPYRD_PLATFORM_EXPOSURE`, default `internal` on cloud, `external` locally).
- Certificates for internal hosts: Let's Encrypt's HTTP-01 cannot reach a private load
  balancer. With a DNS provider configured (RFC-0061) the wildcard certificate covers
  them; without one, internal hosts get certificates from the platform CA (the
  `ca-issuers` component, already used by the local profile), trusted on developer
  machines with `shpyrd cluster trust`. External hosts keep HTTP-01 as today.
- `cluster init` prints the two DNS records to create when no DNS provider is configured
  (`*.<domain>` to the external address; the internal hosts to the internal address, more
  specific records win) and waits for the ones it needs.
- CLI: `shpyrd projects exposure shop internal|external`; `shpyrd.yaml` `exposure:`;
  `shpyrd cluster init --platform-exposure external`.
- Dashboard: an Exposure badge on the project header with the toggle for project admins;
  the cluster page lists both front doors with address, hosts served and certificate
  source.

### Alternatives

- **One load balancer with IP allow-lists.** Simpler, but allow-lists rot and leak the
  platform's existence; a private address is the standard answer in every provider.
- **Internal by default for projects too.** Safer, but a PaaS's first deploy should be
  reachable; projects default to external and the platform to internal.
- **A VPN or identity-aware proxy in front of the platform.** Out of scope; the internal
  load balancer composes with either.

## Design Details

- OKE: the internal Service carries `service.beta.kubernetes.io/oci-load-balancer-internal:
  "true"` and `service.beta.kubernetes.io/oci-load-balancer-subnet1: ${SHPYRD_LB_INTERNAL_SUBNET}`
  (the private LB subnet the OKE scripts create); both Services use the flexible shape
  (`oci-load-balancer-shape: flexible`, min/max bandwidth vars, 10 Mbps default) and
  NSG-based security (`oci.oraclecloud.com/security-rule-management-mode: NSG`) instead of
  security-list management. `SHPYRD_LB_INTERNAL_SUBNET` is required when the platform or any
  project is internal; `cluster init` fails early without it. AWS (RFC-0035):
  `service.beta.kubernetes.io/aws-load-balancer-scheme: internal`.
- `App.spec.exposure` (`external` default) with validation; changing it re-renders the
  Ingress (class and, when the certificate source changes, the issuer annotation) and is
  recorded in the activity feed, not as a release.
- Certificate source per Ingress: wildcard secret via the controller's default certificate
  (RFC-0061) when present, else `letsencrypt` for external hosts and `shpyrd-ca` for
  internal ones; `SHPYRD_INTERNAL_ISSUER` overrides.
- The platform CA on cloud: the `ca-issuers` and `trust-manager` components join cloud
  profiles when anything is internal without a DNS provider; `shpyrd cluster trust` already
  installs the CA locally on macOS and Linux.
- Two-phase `cluster init` (RFC-0035) extends to two addresses; the install record keeps
  both and the summary prints them.
- Status: `App.status.url` is unchanged (hostnames do not move between front doors);
  `App.status.exposure` mirrors the spec once the Ingress is served by the right controller.
- Disabling: `SHPYRD_INTERNAL_LB=false` skips the internal controller; setting a project
  internal then fails with an explanation.

## Open questions

1. Platform internal by default on cloud profiles? Default: yes; `--platform-exposure
   external` for proofs of concept (the OKE PoC keeps external until a VPN exists).
2. Platform CA for internal hosts when no DNS provider is configured, rather than
   requiring one? Default: yes, with a warning at `cluster init` pointing at RFC-0061.
3. Flexible shape at 10 Mbps minimum for both OKE load balancers? Default: yes, both
   configurable.

## Implementation History

- 2026-09-22: RFC written (DNS providers and exposure together).
- 2026-09-24: Rewritten after the OKE proof of concept: DNS providers split out to
  RFC-0061, certificate sources for internal hosts settled, OKE annotations added.
