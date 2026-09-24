#!/usr/bin/env bash
# Writes the kubectl context for the cluster created by ./terraform: the OKE
# kubeconfig for the private endpoint, renamed to oke-<name>, with the server
# pointed at the local end of the Bastion tunnel (tunnel.sh) and TLS still
# verified against the endpoint's own address.
set -euo pipefail
TF_DIR="${TF_DIR:-$(cd "$(dirname "$0")" && pwd)/terraform}"
out() { terraform -chdir="$TF_DIR" output -raw "$1"; }

CLUSTER_ID=$(out cluster_id)
REGION=$(out region)
CONTEXT=$(out kube_context)
API=$(out kubernetes_api_private_endpoint)
API_IP=${API%%:*}

oci ce cluster create-kubeconfig --cluster-id "$CLUSTER_ID" --file "$HOME/.kube/config" \
  --region "$REGION" --token-version 2.0.0 --kube-endpoint PRIVATE_ENDPOINT >/dev/null
current=$(kubectl config current-context)
kubectl config delete-context "$CONTEXT" >/dev/null 2>&1 || true
kubectl config rename-context "$current" "$CONTEXT" >/dev/null
cluster=$(kubectl config view -o jsonpath="{.contexts[?(@.name=='$CONTEXT')].context.cluster}")
kubectl config set-cluster "$cluster" --server "https://127.0.0.1:6443" --tls-server-name "$API_IP" >/dev/null
echo "context $CONTEXT -> https://127.0.0.1:6443 (tunnel to $API); next: $(dirname "$0")/tunnel.sh"
