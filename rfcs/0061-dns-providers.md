# RFC-0061 DNS providers: automatic records and wildcard certificates

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0035 (cloud profiles), RFC-0036 (front doors)

**Creation date:** 2026-09-24

**Last update:** 2026-09-24

## Summary

A `dns` setting on cloud profiles names the provider that hosts the platform's domain
(DNSimple, Cloudflare, Route 53, OCI DNS) and hands it a credential. From then on shpyrd
creates and removes the records for its hostnames — the wildcard for the external front
door, the specific names for internal ones (RFC-0036), later custom domains (RFC-0034) —
and issues one wildcard certificate through DNS-01 that serves every `<name>.<domain>`
host, internal or external, without a certificate per project.

## Motivation

The OKE proof of concept needed a wildcard record created by hand, waited for it, and then
issued one Let's Encrypt certificate per hostname over HTTP-01. That does not scale to
internal hosts (HTTP-01 cannot reach them), to custom domains, or to a team that does not
own the DNS console. The platform knows every hostname it serves; it should publish them.

### Goals

- `shpyrd cluster init --dns dnsimple --dns-token-file ~/.dnsimple-token` and no manual
  record afterwards, on first install and when hosts change.
- One `*.<domain>` certificate, renewed automatically, serving both front doors.
- Provider credentials never in the install record; workload identity where the provider
  offers it.

### Non-Goals

- Split-horizon DNS or private zones; internal hosts are published in the same zone with
  private addresses (the usual practice; the addresses are unreachable from outside
  anyway).
- Registrars, zone creation, delegation: the zone exists and is delegated.
- Custom domains themselves (RFC-0034) — they reuse the providers here.

## Proposal

- `SHPYRD_DNS_PROVIDER=none|dnsimple|cloudflare|route53|oci` with a credential delivered
  like the registry token: `--dns-token-file` (DNSimple, Cloudflare API token), an IAM role
  through IRSA (Route 53), OKE workload identity or an API key (OCI DNS); a hook writes the
  Secret in `shpyrd-system`.
- Records: ExternalDNS (in-tree providers `dnsimple`, `cloudflare`, `aws`, `oci`) watches
  the two front doors' Services and every Ingress; the controller annotates each Ingress
  with the hostnames it serves so records point at the right load balancer. Ownership is
  tracked with ExternalDNS's TXT registry so nothing else in the zone is touched.
- Wildcard certificate: cert-manager solves DNS-01 with the built-in solvers for Cloudflare
  and Route 53 and the DNSimple webhook (`puzzle/cert-manager-webhook-dnsimple`, actively
  maintained); the certificate `*.<domain>` (plus `<domain>`) lives in `shpyrd-system` and
  is served by both ingress-nginx controllers as their default certificate, so project
  Ingresses carry no TLS secret of their own and no issuer annotation. OCI DNS has no
  maintained cert-manager solver: it gets records but certificates keep their RFC-0036
  fallbacks (HTTP-01 external, platform CA internal), stated at `cluster init`.
- `cluster init` no longer prints records or waits for manual DNS when a provider is set;
  it waits for the records it created to resolve.
- Cluster page: a DNS card (provider, zone, records managed, certificate expiry); a
  project's Domains card shows record state per host.

### Alternatives

- **Keep HTTP-01 per host and manual records.** Works for public hosts only and leaves
  internal hosts to the platform CA; kept as the fallback when `dns: none`.
- **cert-manager webhook for OCI DNS.** The two known solvers are stale (last releases
  2021 and 2025 without tags); adopting one means maintaining it. Revisit if Oracle or the
  community picks it up.
- **shpyrd's own DNS client instead of ExternalDNS.** Less to install, but four provider
  APIs to maintain; ExternalDNS already has them and the TXT ownership model.

## Design Details

- Components `external-dns` (Helm, pinned) and `dns01-webhook-dnsimple` (only for
  DNSimple), both in the profile's rc2 with the issuers; a ClusterIssuer
  `letsencrypt-dns01` with the provider's solver; a Certificate `platform-wildcard` in
  `shpyrd-system`; ingress-nginx values `controller.extraArgs.default-ssl-certificate`
  for both controllers.
- Controller: when the wildcard certificate is Ready, project Ingresses omit `tls.secretName`
  and the cert-manager annotation (the default certificate applies); Ingresses for custom
  domains (RFC-0034) keep per-host certificates.
- ExternalDNS: `--source=service,ingress`, `--policy=sync`, `--txt-owner-id=<cluster>`,
  `--domain-filter=<domain>`; the front door Services carry
  `external-dns.alpha.kubernetes.io/hostname: *.<domain>` (external) and nothing
  (internal); internal Ingresses get `external-dns.alpha.kubernetes.io/target: <internal
  address>` from the controller so their A records point at the private load balancer.
- Credentials: DNSimple `DNSIMPLE_OAUTH` (an account token; user tokens also need the
  account id), Cloudflare `CF_API_TOKEN` scoped to the zone, Route 53 IRSA on the
  `external-dns` service account, OCI `oci.yaml` with workload identity
  (`useWorkloadIdentity: true`) or a user API key.
- Rate limits: Let's Encrypt allows 50 certificates per registered domain per week; the
  wildcard reduces the platform to one, so growth in projects no longer counts.
- Disabling: `--dns none` on a later `cluster init` removes ExternalDNS (records stay,
  ownership TXT records are deleted) and returns certificates to the RFC-0036 sources on
  the next renewal.

## Open questions

1. Providers in the first version: DNSimple, Cloudflare, Route 53, OCI DNS (records
   only)? Default: yes; others through ExternalDNS's webhook providers later.
2. Serve the wildcard as the controllers' default certificate rather than copying the
   secret into project namespaces? Default: yes.
3. `--policy=sync` (ExternalDNS deletes records it owns when hosts disappear) or
   `upsert-only`? Default: `sync`, scoped by the TXT owner id.

## Implementation History

- 2026-09-24: RFC written (split from RFC-0036).
