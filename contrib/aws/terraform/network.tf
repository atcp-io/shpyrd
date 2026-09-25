resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = var.name }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = { Name = "${var.name}-igw" }
}

# Public subnets: load balancers and the NAT gateway. The role tag lets the
# cloud provider pick them for internet-facing load balancers.
resource "aws_subnet" "public" {
  count = 2

  vpc_id                  = aws_vpc.this.id
  cidr_block              = local.public_cidrs[count.index]
  availability_zone       = local.azs[count.index]
  map_public_ip_on_launch = true
  tags = {
    Name                                = "${var.name}-public-${local.azs[count.index]}"
    "kubernetes.io/role/elb"            = "1"
    "kubernetes.io/cluster/${var.name}" = "shared"
  }
}

# Private subnets: nodes, pods (VPC CNI), internal load balancers, the VPN
# association and the EFS mount targets.
resource "aws_subnet" "private" {
  count = 2

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.private_cidrs[count.index]
  availability_zone = local.azs[count.index]
  tags = {
    Name                                = "${var.name}-private-${local.azs[count.index]}"
    "kubernetes.io/role/internal-elb"   = "1"
    "kubernetes.io/cluster/${var.name}" = "shared"
  }
}

# NAT gateways: one per availability zone by default, so a zone outage
# leaves the other zone's egress alone; nat_gateway_per_az = false shares a
# single one (a fixed cost saved on a development cluster).
locals {
  nat_count = var.nat_gateway_per_az ? 2 : 1
}

resource "aws_eip" "nat" {
  count  = local.nat_count
  domain = "vpc"
  tags   = { Name = "${var.name}-nat-${local.azs[count.index]}" }
}

resource "aws_nat_gateway" "this" {
  count = local.nat_count

  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id
  tags          = { Name = "${var.name}-nat-${local.azs[count.index]}" }
  depends_on    = [aws_internet_gateway.this]
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }
  tags = { Name = "${var.name}-rt-public" }
}

# One private route table per zone, through that zone's NAT gateway (or
# the shared one).
resource "aws_route_table" "private" {
  count  = 2
  vpc_id = aws_vpc.this.id
  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.this[min(count.index, local.nat_count - 1)].id
  }
  tags = { Name = "${var.name}-rt-private-${local.azs[count.index]}" }
}

resource "aws_route_table_association" "public" {
  count          = 2
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table_association" "private" {
  count          = 2
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[count.index].id
}

# Static addresses of the public front door (RFC-0036): one Elastic IP per
# zone, attached to the Network Load Balancer by the AWS Load Balancer
# Controller (SHPYRD_AWS_LB_EIPS). DNS points at the hostname; the addresses
# are what customers allow-list or point an apex A record at, and they
# survive a rebuild of the load balancer.
resource "aws_eip" "lb" {
  count  = 2
  domain = "vpc"
  tags   = { Name = "${var.name}-lb-${local.azs[count.index]}" }
}
