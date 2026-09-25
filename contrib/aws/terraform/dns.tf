# The platform's public zone (RFC-0061). Delegate it once from the parent
# zone with the name servers in the outputs; ExternalDNS publishes the
# records (the wildcard for the external front door, one per internal
# hostname) and cert-manager solves DNS-01 in it.
resource "aws_route53_zone" "platform" {
  count = var.dns_zone != "" ? 1 : 0

  name    = var.dns_zone
  comment = "shpyrd platform ${var.name}"
  # Records ExternalDNS owned may outlive the cluster (it withdraws them
  # only while it runs); destroy takes them along.
  force_destroy = true
}
