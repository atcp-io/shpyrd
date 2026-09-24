# Network security groups with the rules OKE needs for a private endpoint and
# VCN-native pod networking (docs: "Network Resource Configuration for
# Cluster Creation and Deployment"). One group each for the API endpoint, the
# workers and the pods.

resource "oci_core_network_security_group" "api" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-nsg-api"
}

resource "oci_core_network_security_group" "workers" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-nsg-workers"
}

resource "oci_core_network_security_group" "pods" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-nsg-pods"
}

# Rule shorthand: kind = tcp | all | icmp | svc (TCP 443 to the OCI services
# CIDR through the service gateway); ports apply to tcp.
locals {
  api_rules = {
    "in-workers-6443"   = { dir = "INGRESS", kind = "tcp", cidr = local.cidr.workers, min = 6443, max = 6443, desc = "workers to Kubernetes API" }
    "in-workers-12250"  = { dir = "INGRESS", kind = "tcp", cidr = local.cidr.workers, min = 12250, max = 12250, desc = "kubelet to control plane" }
    "in-workers-icmp"   = { dir = "INGRESS", kind = "icmp", cidr = local.cidr.workers, desc = "path discovery from workers" }
    "in-pods-6443"      = { dir = "INGRESS", kind = "tcp", cidr = local.cidr.pods, min = 6443, max = 6443, desc = "pods to Kubernetes API" }
    "in-pods-12250"     = { dir = "INGRESS", kind = "tcp", cidr = local.cidr.pods, min = 12250, max = 12250, desc = "pods to control plane" }
    "in-bastion-6443"   = { dir = "INGRESS", kind = "tcp", cidr = local.cidr.bastion, min = 6443, max = 6443, desc = "administrators through the Bastion service" }
    "out-services"      = { dir = "EGRESS", kind = "svc", desc = "control plane to OKE" }
    "out-workers-10250" = { dir = "EGRESS", kind = "tcp", cidr = local.cidr.workers, min = 10250, max = 10250, desc = "control plane to kubelet" }
    "out-workers-icmp"  = { dir = "EGRESS", kind = "icmp", cidr = local.cidr.workers, desc = "path discovery to workers" }
    "out-pods-all"      = { dir = "EGRESS", kind = "all", cidr = local.cidr.pods, desc = "control plane to pods" }
  }

  workers_rules = {
    "in-workers-all"         = { dir = "INGRESS", kind = "all", cidr = local.cidr.workers, desc = "worker to worker" }
    "in-pods-all"            = { dir = "INGRESS", kind = "all", cidr = local.cidr.pods, desc = "pods to workers" }
    "in-api-tcp"             = { dir = "INGRESS", kind = "tcp", cidr = local.cidr.api, min = 1, max = 65535, desc = "control plane to workers" }
    "in-any-icmp"            = { dir = "INGRESS", kind = "icmp", cidr = "0.0.0.0/0", desc = "path discovery" }
    "in-lb-public-nodeport"  = { dir = "INGRESS", kind = "tcp", cidr = local.cidr.lb_public, min = 30000, max = 32767, desc = "public load balancers to node ports" }
    "in-lb-public-health"    = { dir = "INGRESS", kind = "tcp", cidr = local.cidr.lb_public, min = 10256, max = 10256, desc = "public load balancer health checks" }
    "in-lb-private-nodeport" = { dir = "INGRESS", kind = "tcp", cidr = local.cidr.lb_private, min = 30000, max = 32767, desc = "private load balancers to node ports" }
    "in-lb-private-health"   = { dir = "INGRESS", kind = "tcp", cidr = local.cidr.lb_private, min = 10256, max = 10256, desc = "private load balancer health checks" }
    "in-bastion-ssh"         = { dir = "INGRESS", kind = "tcp", cidr = local.cidr.bastion, min = 22, max = 22, desc = "ssh through the Bastion service" }
    "out-workers-all"        = { dir = "EGRESS", kind = "all", cidr = local.cidr.workers, desc = "worker to worker" }
    "out-pods-all"           = { dir = "EGRESS", kind = "all", cidr = local.cidr.pods, desc = "workers to pods" }
    "out-services"           = { dir = "EGRESS", kind = "svc", desc = "workers to OCI services (OCIR, OKE)" }
    "out-api-6443"           = { dir = "EGRESS", kind = "tcp", cidr = local.cidr.api, min = 6443, max = 6443, desc = "workers to Kubernetes API" }
    "out-api-12250"          = { dir = "EGRESS", kind = "tcp", cidr = local.cidr.api, min = 12250, max = 12250, desc = "workers to control plane" }
    "out-api-icmp"           = { dir = "EGRESS", kind = "icmp", cidr = local.cidr.api, desc = "path discovery to control plane" }
    "out-internet"           = { dir = "EGRESS", kind = "all", cidr = "0.0.0.0/0", desc = "internet through the NAT gateway (images, Let's Encrypt, drains)" }
  }

  pods_rules = {
    "in-pods-all"     = { dir = "INGRESS", kind = "all", cidr = local.cidr.pods, desc = "pod to pod" }
    "in-workers-all"  = { dir = "INGRESS", kind = "all", cidr = local.cidr.workers, desc = "workers to pods" }
    "in-api-all"      = { dir = "INGRESS", kind = "all", cidr = local.cidr.api, desc = "control plane to pods" }
    "out-pods-all"    = { dir = "EGRESS", kind = "all", cidr = local.cidr.pods, desc = "pod to pod" }
    "out-workers-all" = { dir = "EGRESS", kind = "all", cidr = local.cidr.workers, desc = "pods to workers" }
    "out-services"    = { dir = "EGRESS", kind = "svc", desc = "pods to OCI services" }
    "out-api-6443"    = { dir = "EGRESS", kind = "tcp", cidr = local.cidr.api, min = 6443, max = 6443, desc = "pods to Kubernetes API" }
    "out-api-12250"   = { dir = "EGRESS", kind = "tcp", cidr = local.cidr.api, min = 12250, max = 12250, desc = "pods to control plane" }
    "out-internet"    = { dir = "EGRESS", kind = "all", cidr = "0.0.0.0/0", desc = "internet through the NAT gateway" }
  }

  # One flat map "<group>/<rule>" with a uniform shape (for_each wants a map,
  # and the shorthand above leaves ports and CIDRs out where they do not apply).
  nsg_groups = {
    api     = { id = oci_core_network_security_group.api.id, rules = local.api_rules }
    workers = { id = oci_core_network_security_group.workers.id, rules = local.workers_rules }
    pods    = { id = oci_core_network_security_group.pods.id, rules = local.pods_rules }
  }
  nsg_rules = merge([
    for g, grp in local.nsg_groups : {
      for k, r in grp.rules : "${g}/${k}" => {
        nsg  = grp.id
        dir  = r.dir
        kind = r.kind
        cidr = try(r.cidr, null)
        min  = try(r.min, null)
        max  = try(r.max, null)
        desc = r.desc
      }
    }
  ]...)
}

resource "oci_core_network_security_group_security_rule" "this" {
  for_each = local.nsg_rules

  network_security_group_id = each.value.nsg
  direction                 = each.value.dir
  description               = each.value.desc
  stateless                 = false
  protocol                  = each.value.kind == "tcp" || each.value.kind == "svc" ? "6" : each.value.kind == "icmp" ? "1" : "all"

  source      = each.value.dir == "INGRESS" ? each.value.cidr : null
  source_type = each.value.dir == "INGRESS" ? "CIDR_BLOCK" : null

  destination      = each.value.dir == "EGRESS" ? (each.value.kind == "svc" ? local.services_cidr : each.value.cidr) : null
  destination_type = each.value.dir == "EGRESS" ? (each.value.kind == "svc" ? "SERVICE_CIDR_BLOCK" : "CIDR_BLOCK") : null

  dynamic "tcp_options" {
    for_each = each.value.kind == "tcp" ? [1] : each.value.kind == "svc" ? [2] : []
    content {
      destination_port_range {
        min = each.value.kind == "svc" ? 443 : each.value.min
        max = each.value.kind == "svc" ? 443 : each.value.max
      }
    }
  }

  dynamic "icmp_options" {
    for_each = each.value.kind == "icmp" ? [1] : []
    content {
      type = 3
      code = 4
    }
  }
}
