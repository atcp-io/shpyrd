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

output "kubernetes_api_public" {
  description = "Whether the API has a public endpoint (restricted to admin_cidrs); false means VPN only."
  value       = var.api_public_access
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

output "vpc_id" {
  value = aws_vpc.this.id
}

output "load_balancer_ips" {
  description = "Static addresses of the public front door (one per zone): allow-list them, point apex A records at them."
  value       = aws_eip.lb[*].public_ip
}

output "load_balancer_eip_allocations" {
  description = "Elastic IP allocations the public NLB attaches (--set SHPYRD_AWS_LB_EIPS=...)."
  value       = join(",", aws_eip.lb[*].id)
}

output "registry_ip" {
  description = "ClusterIP of the in-cluster registry inside the Service range (the profile default matches service_cidr = 10.100.0.0/16)."
  value       = local.registry_ip
}

output "next_steps" {
  value = <<-EOT
    ${var.vpn ? "AWS VPN Client > File > Manage Profiles > Add Profile: ${abspath(local_sensitive_file.vpn_profile[0].filename)}${var.api_public_access ? "" : "   # connect: the API is private"}" : "# vpn = true for an access path to the private front door"}
    ../kubeconfig.sh                       # kubectl context eks-${var.name}${var.api_public_access ? " (public endpoint allowed from ${join(", ", local.admin_cidrs)})" : " (private endpoint: VPN connected)"}
    kubectl --context eks-${var.name} get nodes
    shpyrd cluster init --context eks-${var.name} --profile aws --vars-file ${abspath(local_file.shpyrd_vars.filename)} \
      --set SHPYRD_ACME_EMAIL=<email> --enable auth-local
  EOT
}

# Everything the platform needs from the infrastructure, in the file
# `shpyrd cluster init --vars-file` reads, so nobody copies identifiers by
# hand. Not a secret (the VPN profile is the only one, and it stays out).
resource "local_file" "shpyrd_vars" {
  filename        = "${path.module}/${var.name}.vars"
  file_permission = "0644"
  content         = <<-EOT
    # Written by contrib/aws/terraform for shpyrd cluster init --vars-file (flags and --set win over it).
    SHPYRD_DOMAIN=${var.dns_zone != "" ? var.dns_zone : "apps.example.com"}
    SHPYRD_AWS_CLUSTER=${aws_eks_cluster.this.name}
    SHPYRD_AWS_REGION=${var.region}
    SHPYRD_AWS_VPC_ID=${aws_vpc.this.id}
    SHPYRD_AWS_LB_EIPS=${join(",", aws_eip.lb[*].id)}
    SHPYRD_LB_IP=${join(",", aws_eip.lb[*].public_ip)}
    SHPYRD_EFS_ID=${var.shared_storage ? aws_efs_file_system.shared[0].id : ""}
    SHPYRD_DNS_PROVIDER=${var.dns_zone != "" ? "aws" : "none"}
    SHPYRD_DNS_ZONE_ID=${var.dns_zone != "" ? aws_route53_zone.platform[0].zone_id : ""}
    SHPYRD_DNS_REGION=${var.region}
    SHPYRD_BACKUP_TARGET=${var.backup_bucket != "" ? "s3://${var.backup_bucket}/${var.name}" : ""}
    SHPYRD_BACKUP_REGION=${var.backup_bucket != "" ? var.region : ""}
  EOT
}

output "vars_file" {
  description = "Values for shpyrd cluster init --vars-file."
  value       = abspath(local_file.shpyrd_vars.filename)
}
