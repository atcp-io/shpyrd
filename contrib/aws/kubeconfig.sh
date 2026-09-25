#!/usr/bin/env bash
# Writes the kubectl context eks-<name> for the cluster created by ./terraform.
# The API endpoint is private by default: connect the VPN first (the VPC
# resolver answers with private addresses). With api_public_access the
# public endpoint serves the addresses in admin_cidrs without the VPN.
# Authentication is the aws CLI's (aws eks get-token).
set -euo pipefail
TF_DIR="${TF_DIR:-$(cd "$(dirname "$0")" && pwd)/terraform}"
out() { terraform -chdir="$TF_DIR" output -raw "$1"; }

NAME=$(out cluster_name)
REGION=$(out region)
CONTEXT=$(out kube_context)
PROFILE=$(out profile)

args=(--region "$REGION" --name "$NAME" --alias "$CONTEXT")
[ -n "$PROFILE" ] && args+=(--profile "$PROFILE")
aws eks update-kubeconfig "${args[@]}" >/dev/null
if [ "$(out kubernetes_api_public)" = "true" ]; then
  echo "context $CONTEXT -> $(out kubernetes_api_endpoint) (public endpoint, admin_cidrs)"
else
  echo "context $CONTEXT -> $(out kubernetes_api_endpoint) (private endpoint: connect the VPN first)"
fi
