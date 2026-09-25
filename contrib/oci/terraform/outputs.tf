output "cluster_id" {
  value = oci_containerengine_cluster.this.id
}

output "cluster_type" {
  value = var.cluster_type
}

output "region" {
  value = var.region
}

output "kubernetes_api_private_endpoint" {
  description = "Private API endpoint (host:port) reached through the Bastion tunnel."
  value       = oci_containerengine_cluster.this.endpoints[0].private_endpoint
}

output "bastion_id" {
  value = oci_bastion_bastion.this.id
}

output "kube_context" {
  description = "kubectl context name written by ../kubeconfig.sh."
  value       = "oke-${var.name}"
}

output "load_balancer_ip" {
  description = "Reserved public address for the external load balancer; pass as --set SHPYRD_LB_IP=... to shpyrd cluster init."
  value       = var.reserved_public_ip ? oci_core_public_ip.lb[0].ip_address : null
}

output "dns_zone_nameservers" {
  description = "Delegate the zone from its parent with NS records pointing here."
  value       = var.dns_zone != "" ? [for ns in oci_dns_zone.platform[0].nameservers : ns.hostname] : []
}

output "lb_public_subnet_id" {
  value = oci_core_subnet.this["lb_public"].id
}

output "lb_private_subnet_id" {
  description = "Private load balancer subnet for internal front doors (RFC-0036: SHPYRD_LB_INTERNAL_SUBNET)."
  value       = oci_core_subnet.this["lb_private"].id
}

output "pod_cidr" {
  description = "Pass as --set SHPYRD_POD_CIDR=... when it differs from the profile default."
  value       = var.subnet_cidrs.pods
}

output "dns_compartment_ocid" {
  description = "Compartment holding the DNS zone (--dns-compartment)."
  value       = var.dns_zone != "" ? local.compartment_id : null
}

output "dns_zone_ocid" {
  value = var.dns_zone != "" ? oci_dns_zone.platform[0].id : null
}

output "dns_auth" {
  value = var.dns_zone != "" ? var.dns_auth : "none"
}

output "dns_user_ocid" {
  description = "IAM user of the DNS automation (dns_auth = key): --dns-user."
  value       = local.dns_key ? oci_identity_user.dns[0].id : null
}

output "dns_key_fingerprint" {
  value = local.dns_key ? oci_identity_api_key.dns[0].fingerprint : null
}

output "dns_key_file" {
  description = "Private key of the DNS automation user (dns_auth = key): --dns-key-file."
  value       = local.dns_key ? abspath(local_sensitive_file.dns_key[0].filename) : null
}

output "shared_storage_mount_target_id" {
  description = "File Storage mount target for shared volumes (RFC-0060): --set SHPYRD_FSS_MOUNT_TARGET=..."
  value       = var.shared_storage ? oci_file_storage_mount_target.shared[0].id : null
}

output "availability_domain" {
  description = "Availability domain of the nodes and of shared volumes' file systems: --set SHPYRD_FSS_AD=..."
  value       = local.ad
}

# Everything the platform needs from the infrastructure, in the file
# `shpyrd cluster init --vars-file` reads, so nobody copies OCIDs by hand.
# The DNS key stays a separate file (--dns-key-file): it is a secret.
resource "local_file" "shpyrd_vars" {
  filename        = "${path.module}/${var.name}.vars"
  file_permission = "0644"
  content         = <<-EOT
    # Written by contrib/oci/terraform for shpyrd cluster init --vars-file (flags and --set win over it).
    SHPYRD_DOMAIN=${var.dns_zone != "" ? var.dns_zone : "apps.example.com"}
    SHPYRD_LB_IP=${var.reserved_public_ip ? oci_core_public_ip.lb[0].ip_address : ""}
    SHPYRD_INTERNAL_LB_SUBNET=${oci_core_subnet.this["lb_private"].id}
    SHPYRD_FSS_MOUNT_TARGET=${var.shared_storage ? oci_file_storage_mount_target.shared[0].id : ""}
    SHPYRD_FSS_AD=${var.shared_storage ? local.ad : ""}
    SHPYRD_DNS_PROVIDER=${var.dns_zone != "" ? "oci" : "none"}
    SHPYRD_DNS_AUTH=${var.dns_zone != "" ? var.dns_auth : "key"}
    SHPYRD_DNS_COMPARTMENT=${var.dns_zone != "" ? local.compartment_id : ""}
    SHPYRD_DNS_TENANCY=${var.dns_zone != "" ? var.tenancy_ocid : ""}
    SHPYRD_DNS_REGION=${var.dns_zone != "" ? var.region : ""}
    SHPYRD_DNS_USER=${local.dns_key ? oci_identity_user.dns[0].id : ""}
  EOT
}

output "vars_file" {
  description = "Values for shpyrd cluster init --vars-file."
  value       = abspath(local_file.shpyrd_vars.filename)
}

output "vpn_profile" {
  description = "WireGuard profile for this machine (import in the WireGuard app)."
  value       = var.vpn ? abspath(local_sensitive_file.vpn_profile[0].filename) : null
}

output "vpn_enabled" {
  value = var.vpn
}

output "next_steps" {
  value = <<-EOT
    ${var.vpn ? "WireGuard app > Import tunnel(s) from file: ${abspath(local_sensitive_file.vpn_profile[0].filename)}; activate it" : "# vpn = true for an access path without the Bastion tunnel"}
    ../kubeconfig.sh                       # kubeconfig context oke-${var.name}${var.vpn ? " (private endpoint over the VPN)" : " pointed at the tunnel"}
    ${var.vpn ? "" : "../tunnel.sh                           # Bastion session + ssh tunnel 127.0.0.1:6443 (3 hours; run again)"}
    kubectl --context oke-${var.name} get nodes
    shpyrd cluster init --context oke-${var.name} --profile oci --vars-file ${abspath(local_file.shpyrd_vars.filename)} \
      --set SHPYRD_ACME_EMAIL=<email>${local.dns_key ? " --dns-key-file ${abspath(local_sensitive_file.dns_key[0].filename)}" : ""} --enable auth-local
  EOT
}
