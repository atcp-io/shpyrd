# OKE cluster with a private Kubernetes API endpoint (reached through the
# Bastion service), VCN-native pod networking and the public load balancer
# subnet as the default for Services; a private load balancer is chosen per
# Service with the oci-load-balancer-subnet1 annotation (RFC-0036). One node
# pool of flexible-shape workers in the private worker subnet.

resource "oci_containerengine_cluster" "this" {
  compartment_id     = local.compartment_id
  name               = var.name
  kubernetes_version = var.kubernetes_version
  vcn_id             = oci_core_vcn.this.id
  type               = var.cluster_type

  cluster_pod_network_options {
    cni_type = "OCI_VCN_IP_NATIVE"
  }

  endpoint_config {
    subnet_id            = oci_core_subnet.this["api"].id
    is_public_ip_enabled = false
    nsg_ids              = [oci_core_network_security_group.api.id]
  }

  options {
    service_lb_subnet_ids = [oci_core_subnet.this["lb_public"].id]

    kubernetes_network_config {
      services_cidr = var.services_cidr
    }

    add_ons {
      is_kubernetes_dashboard_enabled = false
      is_tiller_enabled               = false
    }
  }

  # NSG rules must exist before the control plane comes up.
  depends_on = [oci_core_network_security_group_security_rule.this]
}

# The OKE node image for this Kubernetes version and the shape's architecture.
data "oci_containerengine_node_pool_option" "this" {
  node_pool_option_id = "all"
  compartment_id      = local.compartment_id
}

locals {
  node_arch = can(regex("A1", var.node_shape)) ? "aarch64" : "x86_64"
  k8s_bare  = trimprefix(var.kubernetes_version, "v")
  node_images = [
    for s in data.oci_containerengine_node_pool_option.this.sources : s.image_id
    if can(regex("OKE-${replace(local.k8s_bare, ".", "\\.")}", s.source_name))
    && can(regex("Oracle-Linux-8", s.source_name))
    && !can(regex("GPU", s.source_name))
    && (local.node_arch == "aarch64" ? can(regex("aarch64", s.source_name)) : !can(regex("aarch64", s.source_name)))
  ]
  node_image_id = length(local.node_images) > 0 ? local.node_images[0] : null
}

resource "oci_containerengine_node_pool" "workers" {
  cluster_id         = oci_containerengine_cluster.this.id
  compartment_id     = local.compartment_id
  name               = "workers"
  kubernetes_version = var.kubernetes_version
  node_shape         = var.node_shape
  ssh_public_key     = local.ssh_public_key

  node_shape_config {
    ocpus         = var.node_ocpus
    memory_in_gbs = var.node_memory_gb
  }

  node_source_details {
    source_type             = "IMAGE"
    image_id                = local.node_image_id
    boot_volume_size_in_gbs = var.node_boot_volume_gb
  }

  node_config_details {
    size    = var.node_count
    nsg_ids = [oci_core_network_security_group.workers.id]

    placement_configs {
      availability_domain = local.ad
      subnet_id           = oci_core_subnet.this["workers"].id
    }

    node_pool_pod_network_option_details {
      cni_type          = "OCI_VCN_IP_NATIVE"
      pod_subnet_ids    = [oci_core_subnet.this["pods"].id]
      pod_nsg_ids       = [oci_core_network_security_group.pods.id]
      max_pods_per_node = var.max_pods_per_node
    }
  }

  lifecycle {
    precondition {
      condition     = local.node_image_id != null
      error_message = "No ${local.node_arch} OKE image for Kubernetes ${var.kubernetes_version} in this region."
    }
  }
}

# Bastion service (free): port-forwarding sessions to the API endpoint and
# ssh to the workers, from the allowed client addresses only.
resource "oci_bastion_bastion" "this" {
  bastion_type                 = "STANDARD"
  compartment_id               = local.compartment_id
  target_subnet_id             = oci_core_subnet.this["bastion"].id
  name                         = local.bastion_name
  client_cidr_block_allow_list = local.admin_cidrs
  max_session_ttl_in_seconds   = 10800
}
