# AWS (EKS) infrastructure for shpyrd

Reference infrastructure for the `aws` profile (RFC-0035): Terraform for the network, the
cluster and the way in, one script for the kubectl context. Copy and adapt; the platform
itself is installed afterwards with `shpyrd cluster init`.

```
terraform/       VPC 10.0.0.0/16 with two public subnets (load balancers, one NAT gateway)
                 and two private /19 subnets (nodes and pods); EKS with the API
                 authentication mode and a private API endpoint (a public one restricted
                 to admin_cidrs with api_public_access); one managed node group (AL2023);
                 the add-ons vpc-cni (network
                 policy on), coredns, kube-proxy, eks-pod-identity-agent, aws-ebs-csi-driver
                 and aws-efs-csi-driver; Pod Identity roles for the CSI drivers, ExternalDNS
                 and cert-manager; optionally the platform's zone in Route 53, an EFS file
                 system for shared volumes and an AWS Client VPN endpoint with its profile
kubeconfig.sh    writes the kubectl context eks-<name> (aws eks update-kubeconfig)
```

## Use

```shell
cd terraform
cp terraform.tfvars.example terraform.tfvars   # profile, region, dns_zone, vpn, shared_storage
terraform init
terraform apply                                # about 15 minutes
cd ..
# AWS VPN Client > File > Manage Profiles > Add Profile > terraform/<name>-vpn.ovpn, connect
./kubeconfig.sh
kubectl --context eks-<name> get nodes         # the name variable, default in variables.tf
```

The Kubernetes API is private: kubectl works with the VPN connected (or from inside the
VPC). `api_public_access = true` adds a public endpoint restricted to `admin_cidrs` for
machines that cannot run the VPN; Terraform refuses to leave a cluster with neither.

`terraform output next_steps` prints the `shpyrd cluster init` command. Terraform writes
`<name>.vars` with every value the platform needs from the infrastructure (domain, Elastic
IPs, EFS, zone); `cluster init --vars-file` reads it, so no identifier is copied by hand,
and the cluster name, VPC and region are discovered from the cluster when the file is
absent. With `dns_zone` set, delegate the zone once
from its parent (NS records from `terraform output dns_zone_nameservers`); ExternalDNS
publishes `*.<zone>` as an alias of the external load balancer as soon as the platform is
up, so no record is written by Terraform.

## Access path: the VPN

With `vpn = true` (the default) Terraform is the certificate authority of an AWS Client VPN
endpoint: it generates the CA, the server certificate (imported to ACM) and one client
certificate, and writes `<name>-vpn.ovpn` next to the state. Import it in the AWS VPN
Client (File > Manage Profiles > Add Profile) and connect: this machine then has a route
into the VPC, so the internal front door (`exposure: internal` projects, or the whole
platform with `--set SHPYRD_PLATFORM_EXPOSURE=internal`) and the cluster's private API
endpoint are reachable, and nothing is exposed for it. The tunnel is split: only the VPC
range goes through it; DNS goes to the VPC resolver so the private endpoint resolves.

The profile is a credential (git-ignored): anyone holding it can connect. Rotate it by
tainting `tls_private_key.vpn_client` and applying.

## Decisions

- **In-tree Network Load Balancers**, not the AWS Load Balancer Controller: nothing in the
  platform needs Elastic IPs or ALB Ingresses yet, and one less controller is one less
  IAM role and webhook. NLBs have hostnames, not addresses; the platform shows hostnames
  and DNS uses aliases.
- **EKS Pod Identity** for every AWS credential (CSI drivers, ExternalDNS, cert-manager):
  roles associated with service accounts, no keys created or stored.
- **VPC CNI network policy** instead of Calico: the CNI's own agent enforces
  `NetworkPolicy` when the add-on runs with `enableNetworkPolicy`.
- **API authentication mode** with the Terraform caller as the first administrator; add
  more with `aws_eks_access_entry`.
- **Private API endpoint by default.** The VPN is the way to kubectl; a public endpoint
  is opt-in and restricted to `admin_cidrs`. Locked out (profile lost, VPN down)?
  `api_public_access = true` and `terraform apply` restores access in two minutes:
  Terraform talks to the AWS control plane, not to Kubernetes.
- **One NAT gateway**: a second doubles a fixed cost for a development cluster.
- **Standard support only**: no extended-support fee; upgrade before the version leaves
  standard support.

## Tear down

`shpyrd cluster destroy --context eks-<name>` removes what the platform created in the
cloud through Kubernetes (projects with their data, load balancers, disks) and waits for
the cloud to confirm, so nothing outlives the cluster; then `terraform destroy` removes
the cluster, the network, the zone and the VPN.

## Caveats

- Costs at the defaults (us-east-1, on demand): EKS control plane $0.10/h, two `t3a.large`
  $0.15/h, NAT gateway $0.045/h plus data, two NLBs $0.045/h, Client VPN association
  $0.10/h plus $0.05/h per connection; about $0.45/h all in, so create a cluster for a
  working session and destroy it after. EBS gp3 $0.08/GB-month, EFS by use, the zone
  $0.50/month.
- With `api_public_access`, the public endpoint admits `admin_cidrs` only (this machine's
  address at apply time by default); when your address changes, `terraform apply` again.
- Managed nodes need a few minutes after creation before add-ons that schedule on them
  (CoreDNS, the CSI controllers) settle; `cluster init` waits for what it needs.
