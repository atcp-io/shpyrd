#!/usr/bin/env bash
# Writes the kubectl context eks-<name> for the cluster created by ./terraform,
# through the public API endpoint (allowed from admin_cidrs) or, with the VPN
# connected, the private one (the VPC resolver answers with private
# addresses). Authentication is the aws CLI's (aws eks get-token).
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
echo "context $CONTEXT -> $(out kubernetes_api_endpoint)"
