# OCI (OKE) proof of concept

Scripts that build the network and cluster of the design document with the
`oci` CLI, re-runnable, recording OCIDs in `.out.env` (ignored by git).

```
create-network.sh   VCN 10.0.0.0/16: private API endpoint, worker, pod and bastion
                    subnets; public and private LB subnets; internet, NAT and service
                    gateways; route tables; NSGs with the OKE rules (VCN-native pods)
create-cluster.sh   OKE basic cluster (private endpoint, VCN-native pod networking),
                    node pool, OCI Bastion service, kubeconfig pointed at the tunnel
tunnel.sh           Bastion port-forwarding session + ssh tunnel 127.0.0.1:6443 -> API
destroy.sh          tears everything down in dependency order
```

Decisions: `BASIC_CLUSTER` (free control plane; upgrade in place if add-ons are
needed); the public LB subnet is the cluster default and private LBs are chosen per
Service with `service.beta.kubernetes.io/oci-load-balancer-subnet1`; `NODE_SHAPE`
defaults to `VM.Standard.E5.Flex` because A1 (Always Free, arm64) and E4 were out of
host capacity in GRU when this was built: set `NODE_SHAPE=VM.Standard.A1.Flex` to try.
Sessions through the Bastion live 3 hours: re-run `tunnel.sh`.
