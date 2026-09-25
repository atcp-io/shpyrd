# Shared (ReadWriteMany) volumes on EFS (RFC-0060): one file system for the
# platform, one access point per volume created by the EFS CSI driver. Mount
# targets in the private subnets admit NFS from the VPC (nodes).
resource "aws_security_group" "efs" {
  count = var.shared_storage ? 1 : 0

  name        = "${var.name}-efs"
  description = "NFS from the nodes to the shared volumes file system"
  vpc_id      = aws_vpc.this.id
  tags        = { Name = "${var.name}-efs" }
}

resource "aws_vpc_security_group_ingress_rule" "efs_nfs" {
  count = var.shared_storage ? 1 : 0

  security_group_id = aws_security_group.efs[0].id
  cidr_ipv4         = var.vpc_cidr
  ip_protocol       = "tcp"
  from_port         = 2049
  to_port           = 2049
  description       = "NFS from the VPC"
}

resource "aws_efs_file_system" "shared" {
  count = var.shared_storage ? 1 : 0

  creation_token   = "${var.name}-shared-volumes"
  encrypted        = true
  throughput_mode  = "elastic"
  performance_mode = "generalPurpose"

  lifecycle_policy {
    transition_to_ia = "AFTER_30_DAYS"
  }

  tags = { Name = "${var.name}-shared-volumes" }
}

resource "aws_efs_mount_target" "shared" {
  count = var.shared_storage ? 2 : 0

  file_system_id  = aws_efs_file_system.shared[0].id
  subnet_id       = aws_subnet.private[count.index].id
  security_groups = [aws_security_group.efs[0].id]
}
