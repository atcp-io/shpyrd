# RFC-0036 Load balancer exposure: internal and external front doors

**Status:** in progress

**Owner:** Patrick Negri

**Owner:** unassigned

**Depends on:** RFC-0035 (cloud profiles); RFC-0061 (DNS providers) for public certificates
on internal hosts

**Creation date:** 2026-09-22

**Last update:** 2026-09-24 (rewritten: DNS providers moved to RFC-0061; OKE specifics; external by default)

## Summary

Cloud profiles get two front doors: an **external** load balancer with a public address
for the applications people publish, and an **internal** one, reachable only from the
company network (VPN, peered VCN, bastion), for anything marked internal: a back-office
project, or the platform itself — dashboard, sign-in, Grafana — once a team has the network
to reach it. Everything is external by default; each project and the platform choose with
one setting. The local profile keeps its single front door and maps both names to it.

## Motivation

The OKE proof of concept publishes everything on the internet because there was one load
balancer. Companies want a choice: a public storefront and an internal back office in the
same platform, and a dashboard that moves behind the company network once a VPN or peering
exists. Both should be a setting, not an infrastructure project.

### Goals

- `exposure: internal | external` per project (`shpyrd.yaml`, CLI, dashboard toggle);
  `SHPYRD_PLATFORM_EXPOSURE=internal` moves the dashboard, sign-in and Grafana behind the
  internal front door.
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
  (`SHPYRD_PLATFORM_EXPOSURE`, default `external` everywhere). The internal controller is
  only installed once something is internal (`SHPYRD_INTERNAL_LB=auto`).
- Certificates for internal hosts: Let's Encrypt's HTTP-01 cannot reach a private load
  balancer. With a DNS provider configured (RFC-0061) the wildcard certificate covers
  them; without one, internal hosts get certificates from the platform CA (the
  `ca-issuers` component, already used by the local profile), trusted on developer
  machines with `shpyrd cluster trust`. External hosts keep HTTP-01 as today.
- `cluster init` prints the two DNS records to create when no DNS provider is configured
  (`*.<domain>` to the external address; the internal hosts to the internal address, more
  specific records win) and waits for the ones it needs.
- CLI: `shpyrd projects exposure shop internal|external`; `shpyrd.yaml` `exposure:`;
  `shpyrd cluster init --platform-exposure internal`.
- Dashboard: an Exposure badge on the project header with the toggle for project admins;
  the cluster page lists both front doors with address, hosts served and certificate
  source.

### Alternatives

- **One load balancer with IP allow-lists.** Simpler, but allow-lists rot and leak the
  platform's existence; a private address is the standard answer in every provider.
- **Internal by default for the platform on cloud.** Safer on paper, but it makes the
  first install depend on a VPN or bastion that a new team rarely has yet, and a dashboard
  nobody can open is not safer. External by default, one setting to move it.
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
- `SHPYRD_INTERNAL_LB=auto|true|false`: `auto` installs the internal controller when the
  platform or a project is internal (a load balancer costs money idle); `false` refuses
  internal exposure with an explanation.

## Open questions

1. Platform CA for internal hosts when no DNS provider is configured, rather than
   requiring one? Default: yes, with a warning at `cluster init` pointing at RFC-0061.
2. Flexible shape at 10 Mbps minimum for both OKE load balancers? Default: yes, both
   configurable.
3. Create the internal load balancer lazily (`SHPYRD_INTERNAL_LB=auto`)? Default: yes.

## Implementation History

- 2026-09-22: RFC written (DNS providers and exposure together).
- 2026-09-24: Rewritten after the OKE proof of concept: DNS providers split out to
  RFC-0061, certificate sources for internal hosts settled, OKE annotations added.
  Decided: external by default for the platform as well; internal is a setting.
