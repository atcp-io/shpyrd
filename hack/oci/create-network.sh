#!/usr/bin/env bash
# Creates the VCN of the design document: one regional VCN, private subnets
# for the API endpoint, workers and pods, a public and a private LB subnet, a
# Bastion subnet; Internet, NAT and Service gateways; route tables; NSGs with
# the rules OKE needs (VCN-native pod networking, private endpoint).
# Re-runnable: existing resources are reused by name.
source "$(dirname "$0")/env.sh"

C="--compartment-id $COMPARTMENT_ID"

note "VCN $POC_NAME ($VCN_CIDR)"
VCN_ID=$(ocid_of "$POC_NAME" oci network vcn list $C)
if [ -z "$VCN_ID" ]; then
  VCN_ID=$(oci network vcn create $C --display-name "$POC_NAME" --cidr-block "$VCN_CIDR" --dns-label shpyrdpoc --wait-for-state AVAILABLE --query data.id --raw-output)
fi
save VCN_ID "$VCN_ID"
V="--vcn-id $VCN_ID"

note "Gateways: internet, NAT, service"
IGW_ID=$(ocid_of igw oci network internet-gateway list $C $V)
[ -n "$IGW_ID" ] || IGW_ID=$(oci network internet-gateway create $C $V --display-name igw --is-enabled true --wait-for-state AVAILABLE --query data.id --raw-output)
NAT_ID=$(ocid_of nat oci network nat-gateway list $C $V)
[ -n "$NAT_ID" ] || NAT_ID=$(oci network nat-gateway create $C $V --display-name nat --wait-for-state AVAILABLE --query data.id --raw-output)
ALL_SERVICES_ID=$(oci network service list --query 'data[?contains(name, `All`)] | [0].id' --raw-output)
ALL_SERVICES_CIDR=$(oci network service list --query 'data[?contains(name, `All`)] | [0]."cidr-block"' --raw-output)
SGW_ID=$(ocid_of sgw oci network service-gateway list $C $V)
[ -n "$SGW_ID" ] || SGW_ID=$(oci network service-gateway create $C $V --display-name sgw --services "[{\"serviceId\":\"$ALL_SERVICES_ID\"}]" --wait-for-state AVAILABLE --query data.id --raw-output)
save IGW_ID "$IGW_ID"; save NAT_ID "$NAT_ID"; save SGW_ID "$SGW_ID"

note "Route tables: public (internet gateway), private (NAT + service gateway)"
RT_PUBLIC_ID=$(ocid_of rt-public oci network route-table list $C $V)
[ -n "$RT_PUBLIC_ID" ] || RT_PUBLIC_ID=$(oci network route-table create $C $V --display-name rt-public --wait-for-state AVAILABLE --query data.id --raw-output \
  --route-rules "[{\"destination\":\"0.0.0.0/0\",\"destinationType\":\"CIDR_BLOCK\",\"networkEntityId\":\"$IGW_ID\"}]")
RT_PRIVATE_ID=$(ocid_of rt-private oci network route-table list $C $V)
[ -n "$RT_PRIVATE_ID" ] || RT_PRIVATE_ID=$(oci network route-table create $C $V --display-name rt-private --wait-for-state AVAILABLE --query data.id --raw-output \
  --route-rules "[{\"destination\":\"0.0.0.0/0\",\"destinationType\":\"CIDR_BLOCK\",\"networkEntityId\":\"$NAT_ID\"},{\"destination\":\"$ALL_SERVICES_CIDR\",\"destinationType\":\"SERVICE_CIDR_BLOCK\",\"networkEntityId\":\"$SGW_ID\"}]")
save RT_PUBLIC_ID "$RT_PUBLIC_ID"; save RT_PRIVATE_ID "$RT_PRIVATE_ID"

note "Security lists (subnet level): NSGs carry the node/pod/endpoint rules; the LB lists carry the client rules"
# tcp rule helper for security lists
sl_tcp() { # sl_tcp <cidr> <min> <max> <desc>
  printf '{"protocol":"6","source":"%s","isStateless":false,"tcpOptions":{"destinationPortRange":{"min":%s,"max":%s}},"description":"%s"}' "$1" "$2" "$3" "$4"
}
sl_egress_tcp() { printf '{"protocol":"6","destination":"%s","isStateless":false,"tcpOptions":{"destinationPortRange":{"min":%s,"max":%s}},"description":"%s"}' "$1" "$2" "$3" "$4"; }
SL_EMPTY_ID=$(ocid_of sl-nsg-only oci network security-list list $C $V)
[ -n "$SL_EMPTY_ID" ] || SL_EMPTY_ID=$(oci network security-list create $C $V --display-name sl-nsg-only --ingress-security-rules '[]' --egress-security-rules '[]' --wait-for-state AVAILABLE --query data.id --raw-output)
SL_LB_PUBLIC_ID=$(ocid_of sl-lb-public oci network security-list list $C $V)
[ -n "$SL_LB_PUBLIC_ID" ] || SL_LB_PUBLIC_ID=$(oci network security-list create $C $V --display-name sl-lb-public --wait-for-state AVAILABLE --query data.id --raw-output \
  --ingress-security-rules "[$(sl_tcp 0.0.0.0/0 80 80 'HTTP from the internet'),$(sl_tcp 0.0.0.0/0 443 443 'HTTPS from the internet')]" \
  --egress-security-rules "[$(sl_egress_tcp $SN_WORKERS_CIDR 30000 32767 'to node ports'),$(sl_egress_tcp $SN_WORKERS_CIDR 10256 10256 'kube-proxy health checks')]")
# The Bastion service is not in an NSG: its subnet list must allow its egress.
SL_BASTION_ID=$(ocid_of sl-bastion oci network security-list list $C $V)
[ -n "$SL_BASTION_ID" ] || SL_BASTION_ID=$(oci network security-list create $C $V --display-name sl-bastion --wait-for-state AVAILABLE --query data.id --raw-output \
  --ingress-security-rules '[]' \
  --egress-security-rules "[$(sl_egress_tcp $SN_API_CIDR 6443 6443 'bastion sessions to the Kubernetes API'),$(sl_egress_tcp $SN_WORKERS_CIDR 22 22 'bastion sessions to worker ssh')]")
SL_LB_PRIVATE_ID=$(ocid_of sl-lb-private oci network security-list list $C $V)
[ -n "$SL_LB_PRIVATE_ID" ] || SL_LB_PRIVATE_ID=$(oci network security-list create $C $V --display-name sl-lb-private --wait-for-state AVAILABLE --query data.id --raw-output \
  --ingress-security-rules "[$(sl_tcp $VCN_CIDR 80 80 'HTTP from the VCN and connected networks'),$(sl_tcp $VCN_CIDR 443 443 'HTTPS from the VCN and connected networks')]" \
  --egress-security-rules "[$(sl_egress_tcp $SN_WORKERS_CIDR 30000 32767 'to node ports'),$(sl_egress_tcp $SN_WORKERS_CIDR 10256 10256 'kube-proxy health checks')]")

note "Subnets (regional)"
mk_subnet() { # mk_subnet <name> <cidr> <dns-label> <route-table> <security-list> <private:true|false>
  local id; id=$(ocid_of "$1" oci network subnet list $C $V)
  if [ -z "$id" ]; then
    id=$(oci network subnet create $C $V --display-name "$1" --cidr-block "$2" --dns-label "$3" --route-table-id "$4" --security-list-ids "[\"$5\"]" --prohibit-public-ip-on-vnic "$6" --wait-for-state AVAILABLE --query data.id --raw-output)
  fi
  echo "$id"
}
SN_API_ID=$(mk_subnet sn-api "$SN_API_CIDR" api "$RT_PRIVATE_ID" "$SL_EMPTY_ID" true)
SN_WORKERS_ID=$(mk_subnet sn-workers "$SN_WORKERS_CIDR" workers "$RT_PRIVATE_ID" "$SL_EMPTY_ID" true)
SN_PODS_ID=$(mk_subnet sn-pods "$SN_PODS_CIDR" pods "$RT_PRIVATE_ID" "$SL_EMPTY_ID" true)
SN_LB_PUBLIC_ID=$(mk_subnet sn-lb-public "$SN_LB_PUBLIC_CIDR" lbpublic "$RT_PUBLIC_ID" "$SL_LB_PUBLIC_ID" false)
SN_LB_PRIVATE_ID=$(mk_subnet sn-lb-private "$SN_LB_PRIVATE_CIDR" lbprivate "$RT_PRIVATE_ID" "$SL_LB_PRIVATE_ID" true)
SN_BASTION_ID=$(mk_subnet sn-bastion "$SN_BASTION_CIDR" bastion "$RT_PRIVATE_ID" "$SL_BASTION_ID" true)
for k in SN_API_ID SN_WORKERS_ID SN_PODS_ID SN_LB_PUBLIC_ID SN_LB_PRIVATE_ID SN_BASTION_ID; do save $k "${!k}"; done

note "Network security groups with the OKE rules (VCN-native pod networking, private endpoint)"
mk_nsg() { local id; id=$(ocid_of "$1" oci network nsg list $C $V); [ -n "$id" ] || id=$(oci network nsg create $C $V --display-name "$1" --wait-for-state AVAILABLE --query data.id --raw-output); echo "$id"; }
NSG_API_ID=$(mk_nsg nsg-api); NSG_WORKERS_ID=$(mk_nsg nsg-workers); NSG_PODS_ID=$(mk_nsg nsg-pods)
save NSG_API_ID "$NSG_API_ID"; save NSG_WORKERS_ID "$NSG_WORKERS_ID"; save NSG_PODS_ID "$NSG_PODS_ID"

# Rule helpers (NSG rules are added only when the NSG has none yet: re-runs do not duplicate).
r_in_tcp()  { printf '{"direction":"INGRESS","protocol":"6","source":"%s","sourceType":"CIDR_BLOCK","isStateless":false,"tcpOptions":{"destinationPortRange":{"min":%s,"max":%s}},"description":"%s"}' "$1" "$2" "$3" "$4"; }
r_in_all()  { printf '{"direction":"INGRESS","protocol":"all","source":"%s","sourceType":"CIDR_BLOCK","isStateless":false,"description":"%s"}' "$1" "$2"; }
r_in_icmp() { printf '{"direction":"INGRESS","protocol":"1","source":"%s","sourceType":"CIDR_BLOCK","isStateless":false,"icmpOptions":{"type":3,"code":4},"description":"%s"}' "$1" "$2"; }
r_out_tcp() { printf '{"direction":"EGRESS","protocol":"6","destination":"%s","destinationType":"CIDR_BLOCK","isStateless":false,"tcpOptions":{"destinationPortRange":{"min":%s,"max":%s}},"description":"%s"}' "$1" "$2" "$3" "$4"; }
r_out_all() { printf '{"direction":"EGRESS","protocol":"all","destination":"%s","destinationType":"CIDR_BLOCK","isStateless":false,"description":"%s"}' "$1" "$2"; }
r_out_svc() { printf '{"direction":"EGRESS","protocol":"6","destination":"%s","destinationType":"SERVICE_CIDR_BLOCK","isStateless":false,"tcpOptions":{"destinationPortRange":{"min":443,"max":443}},"description":"%s"}' "$1" "$2"; }
r_out_icmp(){ printf '{"direction":"EGRESS","protocol":"1","destination":"%s","destinationType":"CIDR_BLOCK","isStateless":false,"icmpOptions":{"type":3,"code":4},"description":"%s"}' "$1" "$2"; }
add_rules() { # add_rules <nsg-id> <json-rules...>
  local id=$1; shift
  local n; n=$(oci network nsg rules list --nsg-id "$id" --query 'length(data)' --raw-output 2>/dev/null || echo 0)
  if [ "${n:-0}" = "0" ]; then
    local IFS=,; oci network nsg rules add --nsg-id "$id" --security-rules "[$*]" >/dev/null
  fi
}

# API endpoint: from workers and pods on 6443/12250, path discovery, admin via Bastion; to workers/pods, OCI services.
add_rules "$NSG_API_ID" \
  "$(r_in_tcp $SN_WORKERS_CIDR 6443 6443 'workers to Kubernetes API')" \
  "$(r_in_tcp $SN_WORKERS_CIDR 12250 12250 'kubelet to control plane')" \
  "$(r_in_icmp $SN_WORKERS_CIDR 'path discovery from workers')" \
  "$(r_in_tcp $SN_PODS_CIDR 6443 6443 'pods to Kubernetes API')" \
  "$(r_in_tcp $SN_PODS_CIDR 12250 12250 'pods to control plane')" \
  "$(r_in_tcp $SN_BASTION_CIDR 6443 6443 'administrators through the Bastion service')" \
  "$(r_out_svc $ALL_SERVICES_CIDR 'control plane to OKE')" \
  "$(r_out_tcp $SN_WORKERS_CIDR 10250 10250 'control plane to kubelet')" \
  "$(r_out_icmp $SN_WORKERS_CIDR 'path discovery to workers')" \
  "$(r_out_all $SN_PODS_CIDR 'control plane to pods')"
# Workers: node to node, control plane in, LB to node ports and health checks, pods; out to everything (NAT), OCI services, API.
add_rules "$NSG_WORKERS_ID" \
  "$(r_in_all $SN_WORKERS_CIDR 'worker to worker')" \
  "$(r_in_all $SN_PODS_CIDR 'pods to workers')" \
  "$(r_in_tcp $SN_API_CIDR 1 65535 'control plane to workers')" \
  "$(r_in_icmp 0.0.0.0/0 'path discovery')" \
  "$(r_in_tcp $SN_LB_PUBLIC_CIDR 30000 32767 'public load balancers to node ports')" \
  "$(r_in_tcp $SN_LB_PUBLIC_CIDR 10256 10256 'public load balancer health checks')" \
  "$(r_in_tcp $SN_LB_PRIVATE_CIDR 30000 32767 'private load balancers to node ports')" \
  "$(r_in_tcp $SN_LB_PRIVATE_CIDR 10256 10256 'private load balancer health checks')" \
  "$(r_in_tcp $SN_BASTION_CIDR 22 22 'ssh through the Bastion service')" \
  "$(r_out_all $SN_WORKERS_CIDR 'worker to worker')" \
  "$(r_out_all $SN_PODS_CIDR 'workers to pods')" \
  "$(r_out_svc $ALL_SERVICES_CIDR 'workers to OCI services (OCIR, OKE)')" \
  "$(r_out_tcp $SN_API_CIDR 6443 6443 'workers to Kubernetes API')" \
  "$(r_out_tcp $SN_API_CIDR 12250 12250 'workers to control plane')" \
  "$(r_out_icmp $SN_API_CIDR 'path discovery to control plane')" \
  "$(r_out_all 0.0.0.0/0 'internet through the NAT gateway (images, Let'"'"'s Encrypt, drains)')"
# Pods: pod to pod, from workers and control plane, LBs; out to everything (NAT), OCI services, API, workers.
add_rules "$NSG_PODS_ID" \
  "$(r_in_all $SN_PODS_CIDR 'pod to pod')" \
  "$(r_in_all $SN_WORKERS_CIDR 'workers to pods')" \
  "$(r_in_all $SN_API_CIDR 'control plane to pods')" \
  "$(r_out_all $SN_PODS_CIDR 'pod to pod')" \
  "$(r_out_all $SN_WORKERS_CIDR 'pods to workers')" \
  "$(r_out_svc $ALL_SERVICES_CIDR 'pods to OCI services')" \
  "$(r_out_tcp $SN_API_CIDR 6443 6443 'pods to Kubernetes API')" \
  "$(r_out_tcp $SN_API_CIDR 12250 12250 'pods to control plane')" \
  "$(r_out_all 0.0.0.0/0 'internet through the NAT gateway')"

note "Network ready. OCIDs in $OUT"
