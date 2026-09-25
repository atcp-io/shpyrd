data "aws_availability_zones" "available" {
  state = "available"
  filter {
    name   = "opt-in-status"
    values = ["opt-in-not-required"]
  }
}

data "aws_caller_identity" "current" {}

# This machine's public address, for the public API endpoint allow list
# when that endpoint is on and admin_cidrs is empty.
data "http" "my_ip" {
  count = var.api_public_access && length(var.admin_cidrs) == 0 ? 1 : 0
  url   = "https://api.ipify.org"
}

locals {
  azs         = slice(data.aws_availability_zones.available.names, 0, 2)
  admin_cidrs = !var.api_public_access ? [] : length(var.admin_cidrs) > 0 ? var.admin_cidrs : ["${trimspace(data.http.my_ip[0].response_body)}/32"]

  # Two public /20 (load balancers, NAT) and two private /19 (nodes and
  # pods) subnets, one pair per availability zone.
  public_cidrs  = [for i in range(2) : cidrsubnet(var.vpc_cidr, 4, i)]
  private_cidrs = [for i in range(2) : cidrsubnet(var.vpc_cidr, 3, i + 2)]

  registry_ip  = cidrhost(var.service_cidr, 50)
  vpc_resolver = cidrhost(var.vpc_cidr, 2)
}
