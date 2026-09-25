# One regional VCN: private subnets for the API endpoint, workers and pods, a
# public and a private load balancer subnet, a Bastion subnet; internet, NAT
# and service gateways. Subnet security lists carry only the client-facing
# rules (load balancers, Bastion egress); the OKE rules live in the NSGs of
# nsg.tf, attached to the endpoint, the workers and the pods.

resource "oci_core_vcn" "this" {
  compartment_id = local.compartment_id
  display_name   = var.name
  cidr_blocks    = [var.vcn_cidr]
  dns_label      = local.dns_label
}

resource "oci_core_internet_gateway" "this" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-igw"
  enabled        = true
}

resource "oci_core_nat_gateway" "this" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-nat"
}

resource "oci_core_service_gateway" "this" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-sgw"

  services {
    service_id = local.services_id
  }
}

resource "oci_core_route_table" "public" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-rt-public"

  route_rules {
    destination       = "0.0.0.0/0"
    destination_type  = "CIDR_BLOCK"
    network_entity_id = oci_core_internet_gateway.this.id
  }
}

resource "oci_core_route_table" "private" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-rt-private"

  route_rules {
    destination       = "0.0.0.0/0"
    destination_type  = "CIDR_BLOCK"
    network_entity_id = oci_core_nat_gateway.this.id
  }

  route_rules {
    destination       = local.services_cidr
    destination_type  = "SERVICE_CIDR_BLOCK"
    network_entity_id = oci_core_service_gateway.this.id
  }
}

# Security lists

resource "oci_core_security_list" "nsg_only" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-sl-nsg-only"
  # Empty on purpose: the NSGs decide.
}

resource "oci_core_security_list" "lb_public" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-sl-lb-public"

  dynamic "ingress_security_rules" {
    for_each = { 80 = "HTTP from the internet", 443 = "HTTPS from the internet" }
    content {
      protocol    = "6"
      source      = "0.0.0.0/0"
      source_type = "CIDR_BLOCK"
      description = ingress_security_rules.value
      tcp_options {
        min = ingress_security_rules.key
        max = ingress_security_rules.key
      }
    }
  }

  egress_security_rules {
    protocol         = "6"
    destination      = local.cidr.workers
    destination_type = "CIDR_BLOCK"
    description      = "to node ports"
    tcp_options {
      min = 30000
      max = 32767
    }
  }

  egress_security_rules {
    protocol         = "6"
    destination      = local.cidr.workers
    destination_type = "CIDR_BLOCK"
    description      = "kube-proxy health checks"
    tcp_options {
      min = 10256
      max = 10256
    }
  }
}

resource "oci_core_security_list" "lb_private" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-sl-lb-private"

  dynamic "ingress_security_rules" {
    for_each = { 80 = "HTTP from the VCN and connected networks", 443 = "HTTPS from the VCN and connected networks" }
    content {
      protocol    = "6"
      source      = var.vcn_cidr
      source_type = "CIDR_BLOCK"
      description = ingress_security_rules.value
      tcp_options {
        min = ingress_security_rules.key
        max = ingress_security_rules.key
      }
    }
  }

  egress_security_rules {
    protocol         = "6"
    destination      = local.cidr.workers
    destination_type = "CIDR_BLOCK"
    description      = "to node ports"
    tcp_options {
      min = 30000
      max = 32767
    }
  }

  egress_security_rules {
    protocol         = "6"
    destination      = local.cidr.workers
    destination_type = "CIDR_BLOCK"
    description      = "kube-proxy health checks"
    tcp_options {
      min = 10256
      max = 10256
    }
  }
}

# The Bastion service has no NSG: its subnet list must allow the sessions' egress.
resource "oci_core_security_list" "bastion" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-sl-bastion"

  egress_security_rules {
    protocol         = "6"
    destination      = local.cidr.api
    destination_type = "CIDR_BLOCK"
    description      = "bastion sessions to the Kubernetes API"
    tcp_options {
      min = 6443
      max = 6443
    }
  }

  egress_security_rules {
    protocol         = "6"
    destination      = local.cidr.workers
    destination_type = "CIDR_BLOCK"
    description      = "bastion sessions to worker ssh"
    tcp_options {
      min = 22
      max = 22
    }
  }

  egress_security_rules {
    protocol         = "6"
    destination      = local.cidr.vpn
    destination_type = "CIDR_BLOCK"
    description      = "bastion sessions to the VPN instance's ssh"
    tcp_options {
      min = 22
      max = 22
    }
  }
}

# Subnets (regional)

locals {
  subnets = {
    api        = { cidr = local.cidr.api, label = "api", rt = oci_core_route_table.private.id, sl = oci_core_security_list.nsg_only.id, private = true }
    workers    = { cidr = local.cidr.workers, label = "workers", rt = oci_core_route_table.private.id, sl = oci_core_security_list.nsg_only.id, private = true }
    pods       = { cidr = local.cidr.pods, label = "pods", rt = oci_core_route_table.private.id, sl = oci_core_security_list.nsg_only.id, private = true }
    lb_public  = { cidr = local.cidr.lb_public, label = "lbpublic", rt = oci_core_route_table.public.id, sl = oci_core_security_list.lb_public.id, private = false }
    lb_private = { cidr = local.cidr.lb_private, label = "lbprivate", rt = oci_core_route_table.private.id, sl = oci_core_security_list.lb_private.id, private = true }
    bastion    = { cidr = local.cidr.bastion, label = "bastion", rt = oci_core_route_table.private.id, sl = oci_core_security_list.bastion.id, private = true }
    vpn        = { cidr = local.cidr.vpn, label = "vpn", rt = oci_core_route_table.public.id, sl = oci_core_security_list.nsg_only.id, private = false }
  }
}

resource "oci_core_subnet" "this" {
  for_each = local.subnets

  compartment_id             = local.compartment_id
  vcn_id                     = oci_core_vcn.this.id
  display_name               = "${var.name}-${replace(each.key, "_", "-")}"
  cidr_block                 = each.value.cidr
  dns_label                  = each.value.label
  route_table_id             = each.value.rt
  security_list_ids          = [each.value.sl]
  prohibit_public_ip_on_vnic = each.value.private
  prohibit_internet_ingress  = each.value.private
}
