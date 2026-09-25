# Access path into the VCN: a small instance running WireGuard (in the
# Oracle Linux 9 kernel), NAT to the VCN. Terraform generates both key
# pairs and writes the client profile the WireGuard app imports. Connected,
# this machine reaches the private API endpoint (no Bastion tunnel), the
# private front door (`--platform-exposure internal`, internal projects) and
# the nodes; nothing else is exposed.
#
# Cost: a Flex shape with 1 OCPU / 2 GB is about three cents an hour; the
# reserved address is free. The Always Free VM.Standard.E2.1.Micro shows
# 500 MB to the guest and cannot run dnf on Oracle Linux 9 (out of memory
# during the install), so it is not the default.

variable "vpn" {
  description = "Create the WireGuard instance and write <name>-vpn.conf for the WireGuard app."
  type        = bool
  default     = true
}

variable "vpn_shape" {
  description = "Shape of the VPN instance; Flex shapes get 1 OCPU / 2 GB. VM.Standard.E2.1.Micro (Always Free) is too small for Oracle Linux 9's package manager."
  type        = string
  default     = "VM.Standard.E5.Flex"
}

variable "vpn_client_cidr" {
  description = "Tunnel addresses (server .1, this machine .2)."
  type        = string
  default     = "10.9.0.0/24"
}

locals {
  vpn_flex   = can(regex("Flex$", var.vpn_shape))
  vpn_server = cidrhost(var.vpn_client_cidr, 1)
  vpn_client = cidrhost(var.vpn_client_cidr, 2)
}

resource "wireguard_asymmetric_key" "server" {
  count = var.vpn ? 1 : 0
}

resource "wireguard_asymmetric_key" "client" {
  count = var.vpn ? 1 : 0
}

resource "oci_core_network_security_group" "vpn" {
  count = var.vpn ? 1 : 0

  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-nsg-vpn"
}

resource "oci_core_network_security_group_security_rule" "vpn_in" {
  count = var.vpn ? 1 : 0

  network_security_group_id = oci_core_network_security_group.vpn[0].id
  direction                 = "INGRESS"
  description               = "WireGuard from anywhere (peers authenticate with keys)"
  protocol                  = "17"
  source                    = "0.0.0.0/0"
  source_type               = "CIDR_BLOCK"
  udp_options {
    destination_port_range {
      min = 51820
      max = 51820
    }
  }
}

resource "oci_core_network_security_group_security_rule" "vpn_ssh" {
  count = var.vpn ? 1 : 0

  network_security_group_id = oci_core_network_security_group.vpn[0].id
  direction                 = "INGRESS"
  description               = "ssh through the Bastion service"
  protocol                  = "6"
  source                    = local.cidr.bastion
  source_type               = "CIDR_BLOCK"
  tcp_options {
    destination_port_range {
      min = 22
      max = 22
    }
  }
}

resource "oci_core_network_security_group_security_rule" "vpn_out" {
  count = var.vpn ? 1 : 0

  network_security_group_id = oci_core_network_security_group.vpn[0].id
  direction                 = "EGRESS"
  description               = "VPN clients to the VCN and the instance to the internet (packages)"
  protocol                  = "all"
  destination               = "0.0.0.0/0"
  destination_type          = "CIDR_BLOCK"
}

data "oci_core_images" "vpn" {
  count = var.vpn ? 1 : 0

  compartment_id           = local.compartment_id
  operating_system         = "Oracle Linux"
  operating_system_version = "9"
  shape                    = var.vpn_shape
  sort_by                  = "TIMECREATED"
  sort_order               = "DESC"
}

resource "oci_core_instance" "vpn" {
  count = var.vpn ? 1 : 0

  availability_domain = local.ad
  compartment_id      = local.compartment_id
  display_name        = "${var.name}-vpn"
  shape               = var.vpn_shape

  dynamic "shape_config" {
    for_each = local.vpn_flex ? [1] : []
    content {
      ocpus         = 1
      memory_in_gbs = 2
    }
  }

  create_vnic_details {
    subnet_id        = oci_core_subnet.this["vpn"].id
    display_name     = "${var.name}-vpn"
    assign_public_ip = false # the reserved address below
    nsg_ids          = [oci_core_network_security_group.vpn[0].id]
    hostname_label   = "vpn"
  }

  source_details {
    source_type = "image"
    source_id   = data.oci_core_images.vpn[0].images[0].id
  }

  metadata = {
    ssh_authorized_keys = local.ssh_public_key
    user_data = base64encode(templatefile("${path.module}/vpn-cloud-init.yaml", {
      server_private = wireguard_asymmetric_key.server[0].private_key
      client_public  = wireguard_asymmetric_key.client[0].public_key
      server_address = "${local.vpn_server}/${split("/", var.vpn_client_cidr)[1]}"
      client_address = local.vpn_client
    }))
  }

  lifecycle {
    ignore_changes = [source_details[0].source_id] # a newer image is not a reason to rebuild
  }
}

data "oci_core_vnic_attachments" "vpn" {
  count = var.vpn ? 1 : 0

  compartment_id = local.compartment_id
  instance_id    = oci_core_instance.vpn[0].id
}

data "oci_core_private_ips" "vpn" {
  count = var.vpn ? 1 : 0

  vnic_id = data.oci_core_vnic_attachments.vpn[0].vnic_attachments[0].vnic_id
}

# Reserved: the profile keeps working when the instance is rebuilt.
resource "oci_core_public_ip" "vpn" {
  count = var.vpn ? 1 : 0

  compartment_id = local.compartment_id
  display_name   = "${var.name}-vpn"
  lifetime       = "RESERVED"
  private_ip_id  = data.oci_core_private_ips.vpn[0].private_ips[0].id
}

# The profile for the WireGuard app (Import tunnel from file). Split tunnel:
# only the VCN goes through it. A credential: keep it with the state.
resource "local_sensitive_file" "vpn_profile" {
  count = var.vpn ? 1 : 0

  filename        = "${path.module}/${var.name}-vpn.conf"
  file_permission = "0600"
  content         = <<-EOT
    [Interface]
    PrivateKey = ${wireguard_asymmetric_key.client[0].private_key}
    Address = ${local.vpn_client}/32

    [Peer]
    PublicKey = ${wireguard_asymmetric_key.server[0].public_key}
    Endpoint = ${oci_core_public_ip.vpn[0].ip_address}:51820
    AllowedIPs = ${var.vcn_cidr}
    PersistentKeepalive = 25
  EOT
}
