# RFC-0061 DNS providers: automatic records and wildcard certificates

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0035 (cloud profiles), RFC-0036 (front doors)

**Creation date:** 2026-09-24

**Last update:** 2026-09-24 (OCI DNS first; Route 53 and Cloudflare later)

## Summary

A `dns` setting on cloud profiles names the provider that hosts the platform's zone and
hands it a credential. From then on shpyrd creates and removes the records for its
hostnames — the wildcard for the external front door, the specific names for internal ones
(RFC-0036), later custom domains (RFC-0034) — and issues one wildcard certificate through
DNS-01 that serves every `<name>.<domain>` host, internal or external, without a
certificate per project. OCI DNS comes first, with the `oci` profile; Route 53 and
Cloudflare follow with their profiles. Registrars that are not providers (DNSimple, for
one) delegate the platform's subdomain to the provider with NS records.

## Motivation

The OKE proof of concept needed a wildcard record created by hand, waited for it, and then
issued one Let's Encrypt certificate per hostname over HTTP-01. That does not scale to
internal hosts (HTTP-01 cannot reach them), to custom domains, or to a team that does not
own the DNS console. The platform knows every hostname it serves; it should publish them.

### Goals

- `shpyrd cluster init --profile oci --domain oci.example.com --dns oci` and no manual
  record afterwards, on first install and when hosts change.
- One `*.<domain>` certificate, renewed automatically, serving both front doors.
- Provider credentials never in the install record; workload identity where the cluster
  offers it.

### Non-Goals

- Split-horizon DNS or private zones; internal hosts are published in the same zone with
  private addresses (usual practice; the addresses are unreachable from outside anyway).
- Registrars, zone creation, delegation: the zone exists in the provider and is delegated.
  The docs show the delegation (NS records at the parent, DNSimple in our case).
- Custom domains themselves (RFC-0034); they reuse the providers here.

## Proposal

- `SHPYRD_DNS_PROVIDER=none|oci|route53|cloudflare`. First version: `oci`. Credential:
  OKE workload identity when the cluster is Enhanced (no secret at all), otherwise one OCI
  API key (`--dns-key-file`, `--dns-user`, fingerprint derived) written by a hook to a
  Secret in `shpyrd-system` and used by both consumers below. Instance principals are not
  offered: every pod on a node can use them unless the policy engine blocks the metadata
  endpoint (RFC-0035 does, but the key stays the narrower credential).
- Records: ExternalDNS (in-tree `oci` provider; `aws`, `cloudflare` later) watches the two
  front doors' Services and every Ingress; the controller annotates each Ingress with the
  hostnames it serves so records point at the right load balancer. Ownership is tracked
  with ExternalDNS's TXT registry so nothing else in the zone is touched.
- Wildcard certificate: cert-manager solves DNS-01 through the OCI DNS webhook solver
  `the-i-engineers/cert-manager-webhook-oci` (actively maintained, Helm chart, workload
  identity or API key), pinned as a component; Route 53 and Cloudflare use cert-manager's
  built-in solvers when their profiles arrive. The certificate `*.<domain>` (plus
  `<domain>`) lives in `shpyrd-system` and is served by both ingress-nginx controllers as
  their default certificate, so project Ingresses carry no TLS secret and no issuer
  annotation.
- `cluster init` no longer prints records or waits for manual DNS when a provider is set;
  it creates the records and waits for them to resolve on public resolvers.
- Cluster page: a DNS card (provider, zone, records managed, certificate expiry); a
  project's Domains card shows record state per host.

### What the operator does

Once, in either case: create the public zone in OCI DNS (`hack/oci/create-dns.sh` does it)
and delegate it at the registrar — for a domain at DNSimple, NS records for the platform
subdomain pointing at the zone's name servers. Then, by cluster type:

| | Enhanced cluster (workload identity) | Basic cluster (API key) |
| --- | --- | --- |
| Credential | none: no key, no Secret | a dedicated IAM user in a `shpyrd-dns` group with an API signing key (`create-dns.sh` creates them; never the operator's own key) |
| IAM policy | `Allow any-user to manage dns in compartment <c> where all {request.principal.type = 'workload', request.principal.cluster_id = '<cluster>', request.principal.service_account = 'external-dns'}` and the same for `dns01-oci` | `Allow group shpyrd-dns to manage dns in compartment <c>` |
| `cluster init` | `--dns oci --dns-compartment <ocid>` | `--dns oci --dns-compartment <ocid> --dns-user <ocid> --dns-key-file <pem>` (fingerprint derived, tenancy and region from the cluster) |
| Rotation | nothing; tokens are short-lived | a new key and `cluster init` again |

`cluster init` prints the policy statement with the cluster OCID filled in, and the cluster
page's DNS card explains a 403 (missing policy, or workload identity on a Basic cluster)
instead of leaving the zone silently empty.

### Alternatives

- **Keep HTTP-01 per host and manual records.** Works for public hosts only and leaves
  internal hosts to the platform CA; kept as the fallback when `dns: none`.
- **DNSimple as a provider.** ExternalDNS supports it and a maintained cert-manager
  webhook exists, so it is cheap to add later; but the platform's domain should live with
  the cloud provider, where workload identity replaces API tokens. Delegating
  `oci.example.com` from DNSimple to OCI DNS with NS records is a one-time step.
- **Writing our own OCI DNS-01 solver** (the API is one `PatchZoneRecords` call). The
  maintained webhook exists and follows cert-manager releases; adopt, contribute if needed.
- **shpyrd's own DNS client instead of ExternalDNS.** Fewer parts, but three provider APIs
  to maintain; ExternalDNS has them and the TXT ownership model.

## Design Details

- Components: `external-dns` (Helm, pinned, `--provider=oci`, `--source=service,ingress`,
  `--policy=sync`, `--txt-owner-id=<cluster>`, `--domain-filter=<domain>`,
  `--oci-zone-scope=GLOBAL`, zones cache on) and `dns01-oci` (the webhook chart), both in
  rc2 with the issuers; a ClusterIssuer `letsencrypt-dns01`; a Certificate
  `platform-wildcard` in `shpyrd-system`; ingress-nginx `controller.extraArgs.default-ssl-certificate`
  on both controllers.
- OCI auth: Enhanced clusters run both with a service account under an IAM policy
  `Allow any-user to manage dns in compartment <c> where all {request.principal.type =
  'workload', request.principal.cluster_id = '<cluster>', request.principal.service_account
  = 'external-dns'}` (and one for the webhook's account); Basic clusters mount the API key
  Secret (`oci.yaml` for ExternalDNS, the profile Secret for the webhook). `cluster init`
  reads the cluster type and picks; the docs carry both policies.
- Controller: when the wildcard certificate is Ready, project Ingresses omit `tls.secretName`
  and the cert-manager annotation; Ingresses for custom domains (RFC-0034) keep per-host
  certificates.
- ExternalDNS: the external front door Service carries
  `external-dns.alpha.kubernetes.io/hostname: *.<domain>`; internal Ingresses get
  `external-dns.alpha.kubernetes.io/target: <internal address>` from the controller so
  their A records point at the private load balancer; ExternalDNS writes one
  `PatchZoneRecords` per zone per sync.
- Zone: a public OCI DNS zone for the platform domain (a subdomain of the company domain,
  delegated from the parent with the zone's NS records; propagation up to 48 hours the
  first time). No per-zone fee; queries are billed per million. `hack/oci` gains
  `create-dns.sh` (zone, policy) and the docs the DNSimple delegation steps.
- Rate limits: Let's Encrypt allows 50 certificates per registered domain per week; the
  wildcard reduces the platform to one.
- Disabling: `--dns none` on a later `cluster init` removes ExternalDNS (records stay,
  ownership TXT records are deleted) and returns certificates to the RFC-0036 sources on
  the next renewal.

## Open questions

1. OCI DNS only in the first version, Route 53 and Cloudflare with their profiles?
   Default: yes.
2. Serve the wildcard as the controllers' default certificate rather than copying the
   secret into project namespaces? Default: yes.
3. `--policy=sync` (ExternalDNS deletes records it owns when hosts disappear) or
   `upsert-only`? Default: `sync`, scoped by the TXT owner id.
4. Verify during implementation that OCI DNS accepts wildcard A records through the API
   (undocumented either way); fallback is per-host records created by ExternalDNS for
   every project, which the Ingress source already yields.

## Implementation History

- 2026-09-24: RFC written (split from RFC-0036); narrowed to OCI DNS first the same day.
