output "cluster_name" {
  value = aws_eks_cluster.this.name
}

output "region" {
  value = var.region
}

output "profile" {
  description = "AWS CLI profile Terraform authenticated with (empty: the environment)."
  value       = var.profile
}

output "kube_context" {
  description = "kubectl context name written by ../kubeconfig.sh."
  value       = "eks-${var.name}"
}

output "kubernetes_api_endpoint" {
  value = aws_eks_cluster.this.endpoint
}

output "dns_zone_id" {
  description = "Route 53 hosted zone of the platform (--dns-zone-id)."
  value       = var.dns_zone != "" ? aws_route53_zone.platform[0].zone_id : null
}

output "dns_zone_nameservers" {
  description = "Delegate the zone from its parent with NS records pointing here."
  value       = var.dns_zone != "" ? aws_route53_zone.platform[0].name_servers : []
}

output "efs_file_system_id" {
  description = "EFS file system behind shared volumes (RFC-0060): --set SHPYRD_EFS_ID=..."
  value       = var.shared_storage ? aws_efs_file_system.shared[0].id : null
}

output "vpn_endpoint_id" {
  value = var.vpn ? aws_ec2_client_vpn_endpoint.this[0].id : null
}

output "vpn_profile" {
  description = "OpenVPN profile for the AWS VPN Client."
  value       = var.vpn ? abspath(local_sensitive_file.vpn_profile[0].filename) : null
}

output "registry_ip" {
  description = "ClusterIP of the in-cluster registry inside the Service range (the profile default matches service_cidr = 10.100.0.0/16)."
  value       = local.registry_ip
}

output "next_steps" {
  value = <<-EOT
    ../kubeconfig.sh                       # kubectl context eks-${var.name} (public endpoint, allowed from ${join(", ", local.admin_cidrs)})
    ${var.vpn ? "AWS VPN Client > File > Manage Profiles > Add Profile: ${abspath(local_sensitive_file.vpn_profile[0].filename)}" : "# vpn = true for an access path to the private front door"}
    kubectl --context eks-${var.name} get nodes
    shpyrd cluster init --context eks-${var.name} --profile aws --domain ${var.dns_zone != "" ? var.dns_zone : "<domain>"} \
      --set SHPYRD_ACME_EMAIL=<email>${var.shared_storage ? " --set SHPYRD_EFS_ID=${aws_efs_file_system.shared[0].id}" : ""}${var.dns_zone != "" ? " \\\n      --dns aws --dns-zone-id ${aws_route53_zone.platform[0].zone_id} --dns-region ${var.region}" : ""}
  EOT
}
