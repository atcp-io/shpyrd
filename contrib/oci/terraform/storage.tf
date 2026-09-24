# Shared (ReadWriteMany) volumes on OCI File Storage (RFC-0060). Block
# volumes cannot be mounted by several instances; the platform's shared
# volumes are NFS file systems that the OKE CSI plugin creates on demand
# behind one mount target. The mount target is free; file systems are billed
# by the space they use.
#
# The mount target lives in the workers subnet behind its own security group
# that admits NFS from the workers only: kubelets mount the exports, pods
# (which have their own addresses in the pods subnet) cannot reach them, so a
# project cannot open another project's shared volume over the network.

# Needs the File Storage service limit "mount-target-count" above zero in the
# availability domain (some tenancies start at 0 in a region: Console >
# Governance > Limits, Quotas and Usage > File Storage > request an increase;
# `oci limits value list --service-name filesystem` shows it).
variable "shared_storage" {
  description = "Create a File Storage mount target for shared (ReadWriteMany) volumes (needs the mount-target-count limit); false leaves shared volumes unavailable."
  type        = bool
  default     = false
}

resource "oci_core_network_security_group" "fss" {
  count = var.shared_storage ? 1 : 0

  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-nsg-fss"
}

locals {
  # NFS from the workers (docs: "Configuring VCN Security Rules for File
  # Storage", scenario A). Stateful rules; replies need no egress rule.
  fss_rules = {
    "in-workers-tcp-111"  = { proto = "6", min = 111, max = 111, desc = "portmapper from workers" }
    "in-workers-tcp-nfs"  = { proto = "6", min = 2048, max = 2050, desc = "NFS from workers" }
    "in-workers-udp-111"  = { proto = "17", min = 111, max = 111, desc = "portmapper from workers" }
    "in-workers-udp-2048" = { proto = "17", min = 2048, max = 2048, desc = "mountd from workers" }
  }
}

resource "oci_core_network_security_group_security_rule" "fss" {
  for_each = var.shared_storage ? local.fss_rules : {}

  network_security_group_id = oci_core_network_security_group.fss[0].id
  direction                 = "INGRESS"
  description               = each.value.desc
  stateless                 = false
  protocol                  = each.value.proto
  source                    = local.cidr.workers
  source_type               = "CIDR_BLOCK"

  dynamic "tcp_options" {
    for_each = each.value.proto == "6" ? [1] : []
    content {
      destination_port_range {
        min = each.value.min
        max = each.value.max
      }
    }
  }

  dynamic "udp_options" {
    for_each = each.value.proto == "17" ? [1] : []
    content {
      destination_port_range {
        min = each.value.min
        max = each.value.max
      }
    }
  }
}

resource "oci_file_storage_mount_target" "shared" {
  count = var.shared_storage ? 1 : 0

  availability_domain = local.ad
  compartment_id      = local.compartment_id
  subnet_id           = oci_core_subnet.this["workers"].id
  display_name        = "${var.name}-shared-volumes"
  nsg_ids             = [oci_core_network_security_group.fss[0].id]
}

# The CSI plugin runs in the OKE control plane as the cluster principal and
# needs leave to create file systems and exports (docs: "Provisioning PVCs
# on the File Storage Service"). Scoped to this cluster.
resource "oci_identity_policy" "shared_storage" {
  count    = var.shared_storage ? 1 : 0
  provider = oci.home

  compartment_id = var.tenancy_ocid
  name           = "${var.name}-shared-volumes"
  description    = "Let the ${var.name} cluster create File Storage file systems for shared volumes"
  statements = [
    "Allow any-user to manage file-family ${local.policy_location} where all {request.principal.type = 'cluster', request.principal.id = '${oci_containerengine_cluster.this.id}'}",
    "Allow any-user to use virtual-network-family ${local.policy_location} where all {request.principal.type = 'cluster', request.principal.id = '${oci_containerengine_cluster.this.id}'}",
  ]
}
