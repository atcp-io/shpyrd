#!/usr/bin/env bash
# Opens a Bastion port-forwarding session to the private Kubernetes API
# endpoint and an SSH tunnel on 127.0.0.1:6443; the kubeconfig context from
# create-cluster.sh points there. Sessions live up to 3 hours: run again.
source "$(dirname "$0")/env.sh"
source "$OUT"
API_IP=${API_PRIVATE_ENDPOINT%%:*}
KEY=${SSH_PUBKEY%.pub}
note "Bastion session to $API_IP:6443"
SESSION_ID=$(oci bastion session create-port-forwarding --bastion-id "$BASTION_ID" --display-name "k8s-$(date +%H%M%S)" \
  --target-private-ip "$API_IP" --target-port 6443 --ssh-public-key-file "$SSH_PUBKEY" --session-ttl 10800 \
  --wait-for-state SUCCEEDED --query 'data.resources[0].identifier' --raw-output)
HOST="host.bastion.$OCI_REGION.oci.oraclecloud.com"
note "tunnel 127.0.0.1:6443 -> $API_IP:6443 (Ctrl-C to close); kubectl --context $KUBE_CONTEXT get nodes"
# A fresh session refuses the key for a few seconds while it propagates.
for attempt in 1 2 3 4 5 6; do
  ssh -i "$KEY" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o ServerAliveInterval=30 -o ExitOnForwardFailure=yes \
    -N -L 6443:"$API_IP":6443 -p 22 "$SESSION_ID@$HOST" && exit 0
  note "ssh exited (attempt $attempt); retrying in 10s"
  sleep 10
done
