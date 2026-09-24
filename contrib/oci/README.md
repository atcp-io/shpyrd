# Oracle Cloud (OKE) infrastructure for shpyrd

Reference infrastructure for the `oci` profile (RFC-0035): Terraform for the network and
the cluster, two small scripts for reaching its private API endpoint. Copy and adapt; the
platform itself is installed afterwards with `shpyrd cluster init`.

```
terraform/       VCN 10.0.0.0/16 with private subnets for the API endpoint, workers and
                 pods, public and private load balancer subnets, a Bastion subnet;
                 internet, NAT and service gateways; NSGs with the OKE rules; the OKE
                 cluster (private endpoint, VCN-native pod networking, Basic or Enhanced),
                 one node pool, the OCI Bastion service, a reserved public address for the
                 load balancer and, optionally, the platform's public zone in OCI DNS
kubeconfig.sh    writes the kubectl context oke-<name> pointed at the tunnel
tunnel.sh        Bastion port-forwarding session + ssh tunnel 127.0.0.1:6443 -> API
```

## Use

Prerequisites: an OCI tenancy with the `oci` CLI configured (`~/.oci/config`), Terraform
1.5+ or OpenTofu, kubectl, an ssh key pair.

```
cd terraform
cp terraform.tfvars.example terraform.tfvars   # tenancy, region, name, shape, dns_zone
terraform init
terraform apply                                # about 15 minutes
cd ..
./kubeconfig.sh
./tunnel.sh &                                  # sessions live 3 hours; run again
kubectl --context oke-shpyrd-dev get nodes
```

`terraform output next_steps` prints the `shpyrd cluster init` command with the reserved
address filled in. With `dns_zone` set, delegate the zone once from its parent (NS records
from `terraform output dns_zone_nameservers`); the wildcard record already points at the
reserved address, so the platform's hostnames resolve as soon as the delegation does.

## Decisions

- **Basic cluster** by default: free control plane; `cluster_type = "ENHANCED_CLUSTER"`
  upgrades in place when workload identity or add-on management is needed.
- **Private API endpoint**, reached through the OCI Bastion service (free) from the
  addresses in `admin_cidrs`; workers are private and reach the internet through the NAT
  gateway and OCI services through the service gateway.
- **VCN-native pod networking** with its own /22 subnet and NSG; `max_pods_per_node` is
  bounded by the shape's VNICs.
- **Public load balancer subnet as the cluster default**; the private one serves internal
  front doors (RFC-0036) through the `oci-load-balancer-subnet1` annotation.
- **Reserved public address** so DNS survives a cluster rebuild; shpyrd passes it to
  ingress-nginx as `SHPYRD_LB_IP`.
- `VM.Standard.E5.Flex` because `A1` (Always Free, arm64) was out of host capacity in the
  region when this was built; set `node_shape = "VM.Standard.A1.Flex"` to try.
- **Shared volumes on File Storage** (`shared_storage = true`, RFC-0060): one mount target
  in the workers subnet behind a security group that admits NFS from the workers only, and
  an IAM policy letting the cluster's CSI plugin create file systems. Pass the two values
  `next_steps` prints (`--set SHPYRD_FSS_MOUNT_TARGET=… --set SHPYRD_FSS_AD=…`) to
  `shpyrd cluster init`. Off by default because it needs the File Storage service limits
  `mount-target-count` and `file-system-count` above zero in the availability domain, which
  some tenancies must request first (Console: Governance > Limits, Quotas and Usage > File
  Storage). The mount target is free; file systems bill by the space used.

## Tear down

`shpyrd cluster destroy --context oke-<name>` removes what the platform created in the
cloud through Kubernetes (projects with their data, load balancers, disks) and waits for
the cloud to confirm, so nothing outlives the cluster; then `terraform destroy` removes
the cluster, the network and the DNS zone.

## Caveats

- OKE does not enforce Kubernetes `NetworkPolicy` with VCN-native pod networking; the
  `oci` profile installs Calico in policy-only mode (RFC-0035).
- Block volumes start at 50 GB; shpyrd rounds smaller requests up and says so (RFC-0060).
  Snapshots are block volume backups (`oci-bv-backup`), billed on the backup's size.
- Costs at the defaults: two E5 workers (2 OCPU / 12 GB) about $0.10 per hour each, a
  flexible load balancer at 10 Mbps; Bastion, VCN, DNS zone and reserved address are free.
