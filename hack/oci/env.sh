#!/usr/bin/env bash
# Variables for the OCI PoC (RFC-0035 counterpart for Oracle Cloud). Source
# from the other scripts. Change the top block; the rest derives.
set -euo pipefail

export OCI_REGION="${OCI_REGION:-sa-saopaulo-1}"
export OCI_REGION_KEY="${OCI_REGION_KEY:-gru}"            # OCIR hostname: gru.ocir.io
export POC_NAME="${POC_NAME:-shpyrd-poc}"
export K8S_VERSION="${K8S_VERSION:-v1.36.1}"
export NODE_COUNT="${NODE_COUNT:-2}"
# VM.Standard.A1.Flex (Ampere, arm64) is the Always Free choice, but A1 hosts
# are often "Out of host capacity" in GRU. VM.Standard.E4.Flex (AMD, amd64)
# (or E5 when E4 is also out) is plentiful, about $0.10/h for 2 x (2 OCPU, 12 GB). Everything
# shpyrd runs is multi-arch; switch back to A1 when capacity exists.
export NODE_SHAPE="${NODE_SHAPE:-VM.Standard.E5.Flex}"
export NODE_POOL_NAME="${NODE_POOL_NAME:-workers}"
export NODE_OCPUS="${NODE_OCPUS:-2}"
export NODE_MEMORY_GB="${NODE_MEMORY_GB:-12}"
export MAX_PODS_PER_NODE="${MAX_PODS_PER_NODE:-31}"       # VCN-native: one pod VNIC on a 2-OCPU shape
export ADMIN_CIDR="${ADMIN_CIDR:-$(curl -fsS https://api.ipify.org)/32}"   # who may open Bastion sessions
export SSH_PUBKEY="${SSH_PUBKEY:-$HOME/.ssh/id_ed25519.pub}"

# Network layout from the design document.
export VCN_CIDR="10.0.0.0/16"
export SN_API_CIDR="10.0.0.0/24"          # Kubernetes API endpoint (private)
export SN_WORKERS_CIDR="10.0.1.0/24"      # ARM worker nodes (private)
export SN_PODS_CIDR="10.0.4.0/22"         # pods, VCN-native networking (private; /22 must sit on a multiple of 4)
export SN_LB_PUBLIC_CIDR="10.0.10.0/24"   # internet-facing load balancers
export SN_LB_PRIVATE_CIDR="10.0.11.0/24"  # internal load balancers
export SN_BASTION_CIDR="10.0.20.0/24"     # OCI Bastion service

# Where the created OCIDs are recorded for the other scripts (and for shpyrd).
export OUT="${OUT:-$(dirname "${BASH_SOURCE[0]}")/.out.env}"

# The tenancy root is the compartment for a PoC; set COMPARTMENT_ID to use another.
export TENANCY_ID="${TENANCY_ID:-$(oci iam availability-domain list --query 'data[0]."compartment-id"' --raw-output)}"
export COMPARTMENT_ID="${COMPARTMENT_ID:-$TENANCY_ID}"
export AD="${AD:-$(oci iam availability-domain list --query 'data[0].name' --raw-output)}"

# oci CLI helpers: look a resource up by name so the scripts can be re-run.
# ocid_of <display-name> <list-cmd...>  (network resources use "display-name")
ocid_of() {
  local name=$1; shift
  "$@" --query "data[?\"display-name\"=='$name' && \"lifecycle-state\"!='TERMINATED' && \"lifecycle-state\"!='TERMINATING' && \"lifecycle-state\"!='DELETED' && \"lifecycle-state\"!='DELETING'] | [0].id" --raw-output 2>/dev/null | grep -v '^null$' || true
}
# ocid_named <name> <list-cmd...>  (OKE resources use "name")
ocid_named() {
  local name=$1; shift
  "$@" --query "data[?name=='$name' && \"lifecycle-state\"!='DELETED' && \"lifecycle-state\"!='DELETING' && \"lifecycle-state\"!='FAILED'] | [0].id" --raw-output 2>/dev/null | grep -v '^null$' || true
}
note() { printf '\033[1;34m==>\033[0m %s\n' "$*" >&2; }
save() { # save KEY VALUE -> appended/replaced in $OUT
  mkdir -p "$(dirname "$OUT")"; touch "$OUT"
  grep -v "^export $1=" "$OUT" > "$OUT.tmp" || true
  printf 'export %s=%q\n' "$1" "$2" >> "$OUT.tmp"; mv "$OUT.tmp" "$OUT"
}
