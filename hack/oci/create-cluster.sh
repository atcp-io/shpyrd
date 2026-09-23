#!/usr/bin/env bash
# Creates the OKE cluster of the design document on the network from
# create-network.sh: private Kubernetes API endpoint, VCN-native pod
# networking, the public LB subnet as the default for Services (a private
# LB is chosen per Service with the oci-load-balancer-subnet1 annotation:
# regional subnets allow one default), an Ampere A1 node
# pool in the private worker subnet, and the OCI Bastion service for
# administrative access. Re-runnable.
source "$(dirname "$0")/env.sh"
source "$OUT"

C="--compartment-id $COMPARTMENT_ID"

note "OKE cluster $POC_NAME ($K8S_VERSION, ${CLUSTER_TYPE:-BASIC_CLUSTER}, VCN-native pods, private endpoint)"
CLUSTER_ID=$(ocid_named "$POC_NAME" oci ce cluster list $C)
if [ -z "$CLUSTER_ID" ]; then
  WR=$(oci ce cluster create $C --name "$POC_NAME" --kubernetes-version "$K8S_VERSION" --vcn-id "$VCN_ID" \
    --type "${CLUSTER_TYPE:-BASIC_CLUSTER}" \
    --cluster-pod-network-options '[{"cniType":"OCI_VCN_IP_NATIVE"}]' \
    --endpoint-subnet-id "$SN_API_ID" --endpoint-public-ip-enabled false --endpoint-nsg-ids "[\"$NSG_API_ID\"]" \
    --service-lb-subnet-ids "[\"$SN_LB_PUBLIC_ID\"]" \
    --query '"opc-work-request-id"' --raw-output)
  note "waiting for the cluster (work request $WR)..."
  oci ce work-request get --work-request-id "$WR" --wait-for-state SUCCEEDED --wait-for-state FAILED --wait-interval-seconds 20 >/dev/null
  CLUSTER_ID=$(ocid_named "$POC_NAME" oci ce cluster list $C)
fi
[ -n "$CLUSTER_ID" ] || { echo "cluster creation failed" >&2; exit 1; }
save CLUSTER_ID "$CLUSTER_ID"
API_PRIVATE=$(oci ce cluster get --cluster-id "$CLUSTER_ID" --query 'data.endpoints."private-endpoint"' --raw-output)
save API_PRIVATE_ENDPOINT "$API_PRIVATE"
note "cluster $CLUSTER_ID, private endpoint $API_PRIVATE"

note "Node pool $NODE_POOL_NAME: $NODE_COUNT x $NODE_SHAPE ($NODE_OCPUS OCPU, ${NODE_MEMORY_GB} GB), private workers, pods in the pod subnet"
# The OKE node image must match the shape's architecture: aarch64 for A1, x86_64 otherwise.
case "$NODE_SHAPE" in *A1*) ARCH=aarch64 ;; *) ARCH=x86_64 ;; esac
IMAGE_ID=$(oci ce node-pool-options get --node-pool-option-id all --query "data.sources[?contains(\"source-name\", \`OKE-${K8S_VERSION#v}\`) && contains(\"source-name\", \`Oracle-Linux-8\`) && ($([ "$ARCH" = aarch64 ] && echo 'contains("source-name", `aarch64`)' || echo '!contains("source-name", `aarch64`) && !contains("source-name", `GPU`)'))] | [0].\"image-id\"" --raw-output)
[ -n "$IMAGE_ID" ] && [ "$IMAGE_ID" != "null" ] || { echo "no $ARCH OKE image for $K8S_VERSION" >&2; exit 1; }
POOL_ID=$(ocid_named "$NODE_POOL_NAME" oci ce node-pool list $C --cluster-id "$CLUSTER_ID")
if [ -z "$POOL_ID" ]; then
  WR=$(oci ce node-pool create $C --cluster-id "$CLUSTER_ID" --name "$NODE_POOL_NAME" --kubernetes-version "$K8S_VERSION" \
    --node-shape "$NODE_SHAPE" --node-shape-config "{\"ocpus\":$NODE_OCPUS,\"memoryInGBs\":$NODE_MEMORY_GB}" \
    --node-source-details "{\"sourceType\":\"IMAGE\",\"imageId\":\"$IMAGE_ID\",\"bootVolumeSizeInGBs\":100}" \
    --size "$NODE_COUNT" \
    --placement-configs "[{\"availabilityDomain\":\"$AD\",\"subnetId\":\"$SN_WORKERS_ID\"}]" \
    --nsg-ids "[\"$NSG_WORKERS_ID\"]" \
    --pod-subnet-ids "[\"$SN_PODS_ID\"]" --pod-nsg-ids "[\"$NSG_PODS_ID\"]" --max-pods-per-node "$MAX_PODS_PER_NODE" \
    --ssh-public-key "$(cat "$SSH_PUBKEY")" \
    --query '"opc-work-request-id"' --raw-output)
  note "waiting for the node pool (work request $WR)..."
  oci ce work-request get --work-request-id "$WR" --wait-for-state SUCCEEDED --wait-for-state FAILED --wait-interval-seconds 20 >/dev/null
  POOL_ID=$(ocid_named "$NODE_POOL_NAME" oci ce node-pool list $C --cluster-id "$CLUSTER_ID")
fi
save POOL_ID "$POOL_ID"

note "Bastion service in the bastion subnet (clients allowed: $ADMIN_CIDR)"
BASTION_ID=$(oci bastion bastion list $C --query "data[?name=='${POC_NAME//-/}' && \"lifecycle-state\"=='ACTIVE'] | [0].id" --raw-output 2>/dev/null | grep -v '^null$' || true)
if [ -z "$BASTION_ID" ]; then
  BASTION_ID=$(oci bastion bastion create $C --bastion-type STANDARD --name "${POC_NAME//-/}" --target-subnet-id "$SN_BASTION_ID" \
    --client-cidr-list "[\"$ADMIN_CIDR\"]" --max-session-ttl 10800 --wait-for-state ACTIVE --query data.id --raw-output)
fi
save BASTION_ID "$BASTION_ID"

note "kubeconfig (private endpoint; reached through the tunnel of tunnel.sh)"
oci ce cluster create-kubeconfig --cluster-id "$CLUSTER_ID" --file "$HOME/.kube/config" --region "$OCI_REGION" --token-version 2.0.0 --kube-endpoint PRIVATE_ENDPOINT
CTX=$(kubectl config current-context)
kubectl config rename-context "$CTX" "oke-$POC_NAME" >/dev/null 2>&1 || true
# The tunnel terminates on the laptop: point the context there and keep TLS verification through the endpoint's own name.
API_IP=${API_PRIVATE%%:*}
kubectl config set-cluster "$(kubectl config view -o jsonpath="{.contexts[?(@.name=='oke-$POC_NAME')].context.cluster}")" --server "https://127.0.0.1:6443" --tls-server-name "$API_IP" >/dev/null
save KUBE_CONTEXT "oke-$POC_NAME"
note "Done. Next: hack/oci/tunnel.sh (keeps a Bastion port-forwarding session to $API_PRIVATE), then kubectl --context oke-$POC_NAME get nodes"
