# RFC-0035 Cloud profiles: Oracle Cloud (OKE) and AWS (EKS)

**Status:** implemented (`oci` and `aws` profiles)

**Owner:** unassigned

**Depends on:** RFC-0045 (implemented); RFC-0034 for custom domains

**Creation date:** 2026-09-22

**Last update:** 2026-09-25 (`aws` profile implemented and verified on EKS)

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

- Provisioning the managed cluster from shpyrd (the Terraform in `contrib/<provider>` and
  the docs do it; a provisioner inside shpyrd is a later RFC).
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
  (load balancer annotations), monitoring and kpack; `contrib/<name>/terraform` for the
  recommended network and cluster; a docs page with the walkthrough, costs and caveats.

### Oracle Cloud (`oci`) — implemented

- Layout created by `contrib/oci/terraform`: VCN 10.0.0.0/16 with subnets for the API endpoint
  (private), workers (private), pods (VCN-native pod networking), public and private load
  balancers and a bastion; internet, NAT and service gateways; NSGs. OKE Basic cluster,
  private endpoint reached through an OCI Bastion port-forward (`contrib/oci/tunnel.sh`),
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
- **Access path (2026-09-25).** A WireGuard instance in a public `vpn` subnet
  (`VM.Standard.E5.Flex`, 1 OCPU / 2 GB; the Always Free micro shows 500 MB to the guest
  and cannot install packages on Oracle Linux 9), keys and the client profile generated
  by Terraform, NAT to the VCN through firewalld, a reserved address so the profile
  survives an instance rebuild. NSG rules admit the VPN subnet to the API endpoint and the
  workers; the private LB already admits the VCN. `kubeconfig.sh` then points at the
  private endpoint directly; the Bastion tunnel remains for `vpn = false`. With it, the
  dev cluster runs the same posture as AWS: API private, dashboard and sign-in on the
  private load balancer (`--platform-exposure internal`), apps public.

### AWS (`aws`)

Reference infrastructure in `contrib/aws/terraform` (mirrors `contrib/oci`), the platform
from `shpyrd cluster init --profile aws`.

- **Network and cluster.** A VPC with two public subnets (load balancers, one NAT gateway
  per zone by default, `nat_gateway_per_az = false` for a single one) and two private /19
  subnets (nodes; the VPC CNI gives pods VPC addresses, so they are
  large), tagged for the in-tree load balancer discovery (`kubernetes.io/role/elb`,
  `internal-elb`). EKS with the API authentication mode (the Terraform caller is the
  first administrator through an access entry), a **private API endpoint only** by
  default: nodes, pods and VPN clients reach it, nothing else does
  (`api_public_access = true` adds a public endpoint restricted to `admin_cidrs` for
  machines without the VPN); standard support only (no extended-support fee), service
  range `10.100.0.0/16`. One managed node group on AL2023 (`t3a.large`, 2 nodes) with containerd.
- **Add-ons instead of components** where EKS ships them: `vpc-cni` with
  `enableNetworkPolicy` (the VPC CNI's own agent enforces `NetworkPolicy`, so
  `SHPYRD_NETWORK_POLICY=none` and `cluster init` recognises `aws-node` as a policy
  engine), `coredns`, `kube-proxy`, `eks-pod-identity-agent`, `aws-ebs-csi-driver` and
  `aws-efs-csi-driver` with Pod Identity roles.
- **Credentials without secrets.** EKS Pod Identity associations give IAM roles to the
  service accounts that need AWS: the two CSI drivers, `shpyrd-system/external-dns` and
  `cert-manager/cert-manager` (Route 53 on the platform's zone only). No access keys are
  created or stored; the OCI API-key flow stays OCI's.
- **Load balancers.** The AWS Load Balancer Controller (component
  `aws-load-balancer-controller`, Pod Identity role from the upstream policy) creates
  Network Load Balancers with **pod targets** for the two ingress-nginx Services: traffic
  goes to the nginx pods, health is per pod, client addresses survive. The public one sits
  on two **Elastic IPs** Terraform reserves (one per zone): the static addresses customers
  allow-list or point an apex A record at, and DNS need not change when the load balancer
  is rebuilt (`SHPYRD_AWS_LB_EIPS` for the controller, `SHPYRD_LB_IP` for the platform's
  A-record hints, both printed by Terraform). The internal one (`aws-load-balancer-scheme:
  internal`, RFC-0036) serves the VPC and VPN clients. Both have hostnames too, which
  ExternalDNS publishes as aliases (`*.<domain>` from the Service annotation, internal
  hostnames per Ingress). An ALB is not offered: it would terminate TLS with ACM
  certificates, which conflicts with cert-manager's per-domain certificates and RFC-0034's
  "point a CNAME and it works"; the `alb` Ingress class stays available for a project that
  needs WAF.
- **DNS (RFC-0061).** `--dns aws --dns-zone-id <id> --dns-region <region>`: ExternalDNS
  with the `aws` provider, the wildcard certificate through cert-manager's built-in Route
  53 DNS-01 solver with ambient (Pod Identity) credentials; the zone is created by
  Terraform when `dns_zone` is set and delegated once from its parent.
- **Storage (RFC-0060).** `gp3` (EBS CSI, encrypted, expansion allowed, the cluster's
  default class since EKS 1.30 marks none), `1Gi` minimum, snapshots with the vendored
  `snapshot-controller` and class `ebs-snapshot`; shared volumes on EFS through access
  points (`shpyrd-efs`: one access point per volume, owned by uid/gid 1000, so the same
  group the platform hands block volumes to writes there without any squash). Terraform
  creates the file system and its mount targets (`shared_storage`, on by default: EFS has
  no service limit to request and bills by use).
- **Registry (RFC-0059).** In-cluster on `10.100.0.50`, 20Gi (`gp3` has no minimum worth
  reserving for); node trust through `registry-nodes` (containerd `certs.d`).
- **Access path.** AWS Client VPN with certificate authentication: Terraform generates
  the CA, server and one client certificate (`tls` provider), imports the server
  certificate to ACM, creates the endpoint (split tunnel to the VPC, the VPC resolver as
  DNS so the private API endpoint resolves), associates one private subnet and writes
  `<name>-vpn.ovpn` next to the state for the AWS VPN Client. With it connected, the
  private front door, the private API endpoint and internal projects are reachable from
  this machine, and so is the API endpoint, which is what makes the private-only API the
  default (Terraform refuses `api_public_access = false` without `vpn = true`). Costs:
  the association is billed per hour while it exists (`vpn = false` removes it),
  connections per hour while connected.
- **Costs at the defaults** (us-east-1, on demand): EKS control plane $0.10/h, two
  `t3a.large` $0.15/h, NAT gateway $0.045/h plus data, two NLBs $0.045/h, Client VPN
  association $0.10/h plus $0.05/h per connection; about $0.45/h all in, so a cluster is
  created for a working session and destroyed after it (`shpyrd cluster destroy
  --context eks-<name>`, then `terraform destroy`).

### Alternatives

- **A provisioner inside shpyrd** (Terraform or the provider SDK). Valuable later; the
  scripts document the layout for now and keep the CLI free of cloud SDKs.
- **Cloud-agnostic ingress through a hosted edge** (Cloudflare Tunnel and the like).
  Interesting for hobby clusters; production platforms want their own load balancer.
- **The in-tree cloud provider's NLBs** (the first version). Node targets through a
  NodePort hop, no static addresses, and a code path AWS has frozen; the controller
  replaced it before the profile shipped.
- **IRSA** instead of Pod Identity. Works on any EKS version but needs the OIDC provider
  and a trust policy per role; Pod Identity is the newer, simpler association and the
  add-ons support it directly.
- **Reaching the private front door through a bastion.** A bastion or SSH port forward
  serves one target and one person; a web platform has many hostnames and many users. The
  VPN gives the machine a route into the VPC, which is what private front doors need.

## Design Details

- Profiles are directories under `deploy/profiles/`; the installer embeds them, so a
  profile change needs a CLI build (`make cli`). Vars are rendered into components with
  `${VAR}` substitution; the install record keeps them (credentials excepted) so later
  `cluster init` runs reuse the domain, front door and registry.
- Waits: load balancer address (`.status.loadBalancer.ingress`), DNS through public
  resolvers (local caches hold negative answers), certificate `Ready`.
- Network policy enforcement is a cluster property, not a profile one, and shpyrd's
  project isolation (RFC-0008) depends on it. OKE with VCN-native pod networking does not
  enforce `NetworkPolicy` at all — verified on the proof of concept, where a pod in one
  project namespace reached another project's instance. Oracle's supported answer is Calico
  in policy-only mode (raw manifest with OKE-specific settings: `FELIX_INTERFACEPREFIX=oci`,
  no default pools, `Append` chain insert mode, the NFT iptables backend on Oracle Linux 8;
  tested Calico version per Kubernetes release, 3.32 for 1.36); it runs on Basic clusters
  and managed node pools. Cilium is not an OKE add-on (Oracle only documents it as a
  flannel replacement on Enhanced clusters) and network security groups on the pod subnet
  are per node pool, not per project. The `oci` profile therefore installs a
  `network-policy` component (Calico policy-only, version matched to the cluster's
  Kubernetes version) by default; `SHPYRD_NETWORK_POLICY=none` skips it when the cluster
  already enforces policies. `cluster init` also detects a policy engine on any profile
  (Calico, Cilium, kindnet from kind 0.24, Antrea, kube-router) and warns when none is
  present.
- Instance metadata: on OCI every pod can use the node's instance principal through the
  metadata endpoint unless something blocks it. Project network policies gain an egress
  deny for `169.254.0.0/16` on cloud profiles (Oracle's own recommendation), which is only
  meaningful with the policy engine above.
- Costs on the proof of concept: two E5 workers at 2 OCPU / 12 GB about $0.10 per hour
  each, a flexible load balancer at 10 Mbps, block volumes from 50 GB (RFC-0060).

## Open questions

1. AWS: existing EKS cluster only (default: yes); a test account is needed before work
   starts. Decided: reference Terraform like OCI's (`contrib/aws`), and the profile works
   on any EKS cluster with the same add-ons and Pod Identity associations.

Decided: Calico policy-only is installed by default on `oci` (`SHPYRD_NETWORK_POLICY=none`
skips it), and project policies deny egress to the link-local range on every profile.

## Implementation History

- 2026-09-22: RFC written as "AWS profile".
- 2026-09-23: `oci` profile implemented and released in v0.1.2/v0.1.3: profile and
  overlays, `letsencrypt-issuers` and `registry-credentials` components, cloud flow in
  `cluster init` (two phases, load balancer and DNS waits), registry credential plumbing in
  the controller, fully qualified images for CRI-O, `hack/oci` scripts (since replaced by
  Terraform); the proof of
  concept runs at `oci.shpyrd.io` with a project on Postgres and Redis.
- 2026-09-24: Retitled to cover cloud profiles in general; OKE network policy finding
  recorded and the `network-policy` component (Calico policy-only) proposed as the `oci`
  default; AWS section kept provisional.
- 2026-09-24: `network-policy` component: the upstream Calico policy-only manifest
  (3.32.0, Oracle's tested version for Kubernetes 1.36) with Oracle's edits for VCN-native
  pod networking as a kustomize patch; on by default for `oci`. Project policies deny
  egress to 169.254.0.0/16 (instance metadata). `cluster init` warns when no policy engine
  is found. Verified on the proof of concept: a pod in another project can no longer reach
  a shop instance or its database, instance metadata is unreachable from project pods,
  and internet, registry, ingress and builds keep working.
- 2026-09-24: Infrastructure moved from `oci` CLI scripts to Terraform in
  `contrib/oci/terraform` (plain resources, OpenTofu-compatible), with a reserved public
  address for the load balancer (`SHPYRD_LB_IP`) and an optional OCI DNS zone with the
  wildcard record; the proof of concept was rebuilt from it in one apply.
- 2026-09-25: AWS design written after the OCI phase: EKS with Pod Identity, in-tree
  NLBs, Route 53 through ExternalDNS and cert-manager's solver, gp3/EFS/snapshots, Client
  VPN as the access path. Implementation starts.
- 2026-09-25: `aws` profile implemented and verified on a dev cluster in us-east-1:
  private API through the Client VPN, dashboard and sign-in behind the internal NLB, apps
  on the public NLB's Elastic IPs, Route 53 records and wildcard certificate through Pod
  Identity, gp3/EFS/snapshots, Postgres and Redis. Released in v0.3.1.
