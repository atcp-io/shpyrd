# RFC-0035 Cloud profiles: Oracle Cloud (OKE) and AWS (EKS)

**Status:** implemented (`oci` profile); AWS provisional

**Owner:** unassigned (AWS)

**Depends on:** RFC-0045 (implemented); RFC-0034 for custom domains

**Creation date:** 2026-09-22

**Last update:** 2026-09-24 (retitled from "AWS profile": the first cloud target became OKE)

## Summary

`shpyrd cluster init --profile <cloud> --domain apps.example.com` installs the platform on
an existing managed Kubernetes cluster. A cloud profile is the local profile with three
substitutions — a cloud load balancer instead of host ports, publicly trusted certificates
instead of the development CA, and the provider's storage — plus whatever the provider
needs for builds to push and nodes to pull. The `oci` profile (Oracle Kubernetes Engine)
is implemented and runs the proof of concept at `oci.shpyrd.io`; `aws` (EKS) follows the
same shape and waits for a test account.

## Motivation

The local profile proves the product; a cloud profile is where teams run it. Oracle Cloud
went first for cost (the free tier and cheap flexible shapes) and because it exercises the
harder path: private API endpoint, private workers, CRI-O nodes, a provider registry.

### Goals

- One command on an existing cluster given the network and DNS prerequisites, with the
  waits and checks that make the first run succeed (load balancer address, DNS
  propagation, certificate issuance).
- The controller and CLI behave the same on every profile; differences live in
  `deploy/profiles/<name>` and a few vars.
- Infrastructure scripts and documentation per provider for the recommended layout.

### Non-Goals

- Provisioning the managed cluster from shpyrd (the scripts in `hack/<provider>` and the
  docs do it; a proper provisioner is a later RFC).
- Provider-managed databases (RDS, OCI Database) as Postgres resources.

## Proposal

- Profile vars: `SHPYRD_FRONT_DOOR=lb` (URLs without ports, the load balancer terminates
  nothing), `SHPYRD_CLUSTER_ISSUER=letsencrypt` with `SHPYRD_ACME_EMAIL`, the registry
  (`SHPYRD_REGISTRY_HOST`, `SHPYRD_REGISTRY_SECRET`, `SHPYRD_REGISTRY_INSECURE`; RFC-0059
  makes the in-cluster registry the default), the pod CIDR for project network policies,
  storage classes (RFC-0060).
- Components shared by cloud profiles: `letsencrypt-issuers` (HTTP-01 ClusterIssuer),
  `registry-credentials` (a hook that writes the `shpyrd-registry` pull/push Secret from
  `--registry-user` and `--registry-token-file`, never into the install record).
- `cluster init` on a cloud profile skips the kind checks, applies everything except the
  platform, waits for the ingress load balancer address, prints the wildcard record to
  create, waits for it to resolve on public resolvers, then applies the rest and waits for
  certificates.
- Controller: mirrors the registry Secret into project namespaces, runs kpack builds as a
  `shpyrd-builder` service account carrying it, sets `imagePullSecrets` on Deployments, run
  pods and build Jobs, mounts a docker config for BuildKit; every platform image reference
  is fully qualified (CRI-O enforces short-name rules).
- Per provider: `deploy/profiles/<name>/profile.yaml` with overlays for ingress-nginx
  (load balancer annotations), monitoring and kpack; `hack/<name>/` scripts for the
  recommended network and cluster; a docs page with the walkthrough, costs and caveats.

### Oracle Cloud (`oci`) — implemented

- Layout created by `hack/oci`: VCN 10.0.0.0/16 with subnets for the API endpoint
  (private), workers (private), pods (VCN-native pod networking), public and private load
  balancers and a bastion; internet, NAT and service gateways; NSGs. OKE Basic cluster,
  private endpoint reached through an OCI Bastion port-forward (`hack/oci/tunnel.sh`),
  one node pool of `VM.Standard.E5.Flex` workers (ARM `A1` shapes were out of capacity in
  the region).
- Registry: OCIR in the proof of concept (`<region>.ocir.io/<tenancy-namespace>`, auth
  token, repositories created private on first push); RFC-0059 replaces it as the default
  with the in-cluster registry.
- Load balancer: OCI flexible load balancer created by ingress-nginx's Service; OKE accepts
  one regional subnet as the default LB subnet; private load balancers need the subnet
  annotation (RFC-0036).
- Nodes run CRI-O on Oracle Linux 8; the kubelet's short-name enforcement surfaced every
  unqualified image reference (Valkey, probes) and they are qualified now.

### AWS (`aws`) — provisional

- EKS with the AWS Load Balancer Controller (NLB for ingress-nginx), `gp3` and `efs`
  storage classes, ECR through an IRSA role for builds or the in-cluster registry
  (RFC-0059), Route 53 through RFC-0061; the EKS OIDC link so the RBAC mirror applies to
  `kubectl` users. Prerequisites checked by `cluster init`: IAM roles, the cluster's OIDC
  provider. Documentation: an eksctl example, IAM policies, costs. Blocked on a test
  account.

### Alternatives

- **A provisioner inside shpyrd** (Terraform or the provider SDK). Valuable later; the
  scripts document the layout for now and keep the CLI free of cloud SDKs.
- **Cloud-agnostic ingress through a hosted edge** (Cloudflare Tunnel and the like).
  Interesting for hobby clusters; production platforms want their own load balancer.

## Design Details

- Profiles are directories under `deploy/profiles/`; the installer embeds them, so a
  profile change needs a CLI build (`make cli`). Vars are rendered into components with
  `${VAR}` substitution; the install record keeps them (credentials excepted) so later
  `cluster init` runs reuse the domain, front door and registry.
- Waits: load balancer address (`.status.loadBalancer.ingress`), DNS through public
  resolvers (local caches hold negative answers), certificate `Ready`.
- Network policy enforcement is a cluster property, not a profile one: OKE with VCN-native
  pod networking does not enforce `NetworkPolicy` unless the Cilium add-on (Enhanced
  clusters) or Calico is installed — verified on the proof of concept, where a pod in one
  project namespace reached another project's instance. `cluster init` detects a policy
  engine (Calico, Cilium, kube-router, kindnet with network policies, Antrea) and warns
  when none is present, with the provider's instructions; project isolation (RFC-0008) is
  documented as requiring one.
- Costs on the proof of concept: two E5 workers at 2 OCPU / 12 GB about $0.10 per hour
  each, a flexible load balancer at 10 Mbps, block volumes from 50 GB (RFC-0060).

## Open questions

1. Warn about a missing network policy engine, or install Calico in policy-only mode as an
   optional component? Default: warn and document now; a `network-policy` component when
   a second provider needs it.
2. AWS: existing EKS cluster only (default: yes); a test account is needed before work
   starts.

## Implementation History

- 2026-09-22: RFC written as "AWS profile".
- 2026-09-23: `oci` profile implemented and released in v0.1.2/v0.1.3: profile and
  overlays, `letsencrypt-issuers` and `registry-credentials` components, cloud flow in
  `cluster init` (two phases, load balancer and DNS waits), registry credential plumbing in
  the controller, fully qualified images for CRI-O, `hack/oci` scripts; the proof of
  concept runs at `oci.shpyrd.io` with a project on Postgres and Redis.
- 2026-09-24: Retitled to cover cloud profiles in general; OKE network policy finding
  recorded; AWS section kept provisional.
