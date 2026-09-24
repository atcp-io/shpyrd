# A reserved public address for the external load balancer (ingress-nginx
# takes it through spec.loadBalancerIP; shpyrd: --set SHPYRD_LB_IP=<address>)
# and, optionally, the platform's public zone in OCI DNS with the wildcard
# record pointing at it. Delegate the zone from its parent with the NS records
# in the outputs; RFC-0061 later manages the records from inside the cluster.

resource "oci_core_public_ip" "lb" {
  count = var.reserved_public_ip ? 1 : 0

  compartment_id = local.compartment_id
  display_name   = "${var.name}-lb"
  lifetime       = "RESERVED"

  lifecycle {
    # The load balancer holds the address; Terraform must not fight over it.
    ignore_changes = [private_ip_id]
  }
}

resource "oci_dns_zone" "platform" {
  count = var.dns_zone != "" ? 1 : 0

  compartment_id = local.compartment_id
  name           = var.dns_zone
  zone_type      = "PRIMARY"
  scope          = "GLOBAL"
}

resource "oci_dns_rrset" "wildcard" {
  count = var.dns_zone != "" && var.reserved_public_ip ? 1 : 0

  zone_name_or_id = oci_dns_zone.platform[0].id
  domain          = "*.${var.dns_zone}"
  rtype           = "A"
  compartment_id  = local.compartment_id

  items {
    domain = "*.${var.dns_zone}"
    rtype  = "A"
    rdata  = oci_core_public_ip.lb[0].ip_address
    ttl    = 300
  }
}
