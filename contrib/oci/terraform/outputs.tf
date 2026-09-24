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

output "next_steps" {
  value = <<-EOT
    ../kubeconfig.sh                       # kubeconfig context oke-${var.name} pointed at the tunnel
    ../tunnel.sh                           # Bastion session + ssh tunnel 127.0.0.1:6443 (3 hours; run again)
    kubectl --context oke-${var.name} get nodes
    shpyrd cluster init --context oke-${var.name} --profile oci --domain <domain> \
      --set SHPYRD_ACME_EMAIL=<email>${var.reserved_public_ip ? " --set SHPYRD_LB_IP=${oci_core_public_ip.lb[0].ip_address}" : ""} \
      --set SHPYRD_REGISTRY_HOST=<region-key>.ocir.io/<tenancy-namespace> --registry-user <namespace>/<user> --registry-token-file <file>
  EOT
}
