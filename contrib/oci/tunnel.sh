#!/usr/bin/env bash
# Opens a Bastion port-forwarding session to the private Kubernetes API
# endpoint of the cluster created by ./terraform and keeps an ssh tunnel on
# 127.0.0.1:6443, where the kubeconfig context from kubeconfig.sh points.
# Sessions live up to 3 hours: run again. SSH_KEY selects the key pair
# (default: the one Terraform installed on the workers).
set -euo pipefail
TF_DIR="${TF_DIR:-$(cd "$(dirname "$0")" && pwd)/terraform}"
out() { terraform -chdir="$TF_DIR" output -raw "$1"; }
note() { printf '\033[1;34m==>\033[0m %s\n' "$*" >&2; }

BASTION_ID=$(out bastion_id)
REGION=$(out region)
CONTEXT=$(out kube_context)
API=$(out kubernetes_api_private_endpoint)
API_IP=${API%%:*}
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519}"

note "Bastion session to $API_IP:6443"
SESSION_ID=$(oci bastion session create-port-forwarding --bastion-id "$BASTION_ID" --display-name "k8s-$(date +%H%M%S)" \
  --target-private-ip "$API_IP" --target-port 6443 --ssh-public-key-file "$SSH_KEY.pub" --session-ttl 10800 \
  --wait-for-state SUCCEEDED --query 'data.resources[0].identifier' --raw-output)
HOST="host.bastion.$REGION.oci.oraclecloud.com"
note "tunnel 127.0.0.1:6443 -> $API_IP:6443 (Ctrl-C to close); kubectl --context $CONTEXT get nodes"
# A fresh session refuses the key for a few seconds while it propagates.
for attempt in 1 2 3 4 5 6; do
  ssh -i "$SSH_KEY" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o ServerAliveInterval=30 -o ExitOnForwardFailure=yes \
    -N -L 6443:"$API_IP":6443 -p 22 "$SESSION_ID@$HOST" && exit 0
  note "ssh exited (attempt $attempt); retrying in 10s"
  sleep 10
done
