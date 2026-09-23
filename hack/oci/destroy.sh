#!/usr/bin/env bash
# Tears the PoC down in dependency order. Load balancers created by
# Kubernetes Services must be gone first (delete the Services or the cluster
# does it on deletion).
source "$(dirname "$0")/env.sh"
source "$OUT" 2>/dev/null || { echo "nothing recorded in $OUT" >&2; exit 1; }
C="--compartment-id $COMPARTMENT_ID"
[ -n "${BASTION_ID:-}" ] && { note "bastion"; oci bastion bastion delete --bastion-id "$BASTION_ID" --force --wait-for-state DELETED || true; }
[ -n "${POOL_ID:-}" ] && { note "node pool"; oci ce node-pool delete --node-pool-id "$POOL_ID" --force --wait-for-state SUCCEEDED || true; }
[ -n "${CLUSTER_ID:-}" ] && { note "cluster"; oci ce cluster delete --cluster-id "$CLUSTER_ID" --force --wait-for-state SUCCEEDED || true; }
for s in SN_API_ID SN_WORKERS_ID SN_PODS_ID SN_LB_PUBLIC_ID SN_LB_PRIVATE_ID SN_BASTION_ID; do
  [ -n "${!s:-}" ] && { note "$s"; oci network subnet delete --subnet-id "${!s}" --force --wait-for-state TERMINATED || true; }
done
for n in NSG_API_ID NSG_WORKERS_ID NSG_PODS_ID; do [ -n "${!n:-}" ] && oci network nsg delete --nsg-id "${!n}" --force --wait-for-state TERMINATED || true; done
for r in RT_PUBLIC_ID RT_PRIVATE_ID; do [ -n "${!r:-}" ] && oci network route-table delete --rt-id "${!r}" --force --wait-for-state TERMINATED || true; done
[ -n "${IGW_ID:-}" ] && oci network internet-gateway delete --ig-id "$IGW_ID" --force --wait-for-state TERMINATED || true
[ -n "${NAT_ID:-}" ] && oci network nat-gateway delete --nat-gateway-id "$NAT_ID" --force --wait-for-state TERMINATED || true
[ -n "${SGW_ID:-}" ] && oci network service-gateway delete --service-gateway-id "$SGW_ID" --force --wait-for-state TERMINATED || true
for l in $(oci network security-list list $C --vcn-id "$VCN_ID" --query 'data[?"display-name"!=`Default Security List for '"$POC_NAME"'`].id' --raw-output | tr -d '[]", '); do oci network security-list delete --security-list-id "$l" --force --wait-for-state TERMINATED || true; done
[ -n "${VCN_ID:-}" ] && { note "vcn"; oci network vcn delete --vcn-id "$VCN_ID" --force --wait-for-state TERMINATED || true; }
rm -f "$OUT"; note "gone"
