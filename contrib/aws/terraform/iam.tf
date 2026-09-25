# EKS Pod Identity: IAM roles handed to Kubernetes service accounts through
# associations, no keys anywhere. One role per consumer, each with only what
# it does.

data "aws_iam_policy_document" "pod_identity_assume" {
  statement {
    actions = ["sts:AssumeRole", "sts:TagSession"]
    principals {
      type        = "Service"
      identifiers = ["pods.eks.amazonaws.com"]
    }
  }
}

# --- CSI drivers -------------------------------------------------------------

resource "aws_iam_role" "ebs_csi" {
  name               = "${var.name}-ebs-csi"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_assume.json
}

resource "aws_iam_role_policy_attachment" "ebs_csi" {
  role       = aws_iam_role.ebs_csi.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
}

resource "aws_iam_role" "efs_csi" {
  count              = var.shared_storage ? 1 : 0
  name               = "${var.name}-efs-csi"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_assume.json
}

resource "aws_iam_role_policy_attachment" "efs_csi" {
  count      = var.shared_storage ? 1 : 0
  role       = aws_iam_role.efs_csi[0].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonEFSCSIDriverPolicy"
}

# --- DNS automation (RFC-0061): ExternalDNS and cert-manager on the zone -----

data "aws_iam_policy_document" "dns" {
  count = var.dns_zone != "" ? 1 : 0

  statement {
    actions   = ["route53:ChangeResourceRecordSets", "route53:ListResourceRecordSets", "route53:ListTagsForResource"]
    resources = [aws_route53_zone.platform[0].arn]
  }
  statement {
    actions   = ["route53:ListHostedZones", "route53:ListHostedZonesByName", "route53:GetChange"]
    resources = ["*"]
  }
}

resource "aws_iam_role" "dns" {
  for_each = var.dns_zone != "" ? toset(["external-dns", "cert-manager"]) : toset([])

  name               = "${var.name}-${each.key}"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_assume.json
}

resource "aws_iam_role_policy" "dns" {
  for_each = aws_iam_role.dns

  name   = "route53-${var.dns_zone}"
  role   = each.value.name
  policy = data.aws_iam_policy_document.dns[0].json
}

resource "aws_eks_pod_identity_association" "external_dns" {
  count = var.dns_zone != "" ? 1 : 0

  cluster_name    = aws_eks_cluster.this.name
  namespace       = "shpyrd-system"
  service_account = "external-dns"
  role_arn        = aws_iam_role.dns["external-dns"].arn
  depends_on      = [aws_eks_addon.pod_identity]
}

resource "aws_eks_pod_identity_association" "cert_manager" {
  count = var.dns_zone != "" ? 1 : 0

  cluster_name    = aws_eks_cluster.this.name
  namespace       = "cert-manager"
  service_account = "cert-manager"
  role_arn        = aws_iam_role.dns["cert-manager"].arn
  depends_on      = [aws_eks_addon.pod_identity]
}

# --- AWS Load Balancer Controller (RFC-0035) ---------------------------------
# The upstream policy for the pinned controller version (iam/), unchanged.

resource "aws_iam_role" "lb_controller" {
  name               = "${var.name}-aws-load-balancer-controller"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_assume.json
}

resource "aws_iam_role_policy" "lb_controller" {
  name   = "aws-load-balancer-controller"
  role   = aws_iam_role.lb_controller.name
  policy = file("${path.module}/iam/aws-load-balancer-controller.json")
}

resource "aws_eks_pod_identity_association" "lb_controller" {
  cluster_name    = aws_eks_cluster.this.name
  namespace       = "kube-system"
  service_account = "aws-load-balancer-controller"
  role_arn        = aws_iam_role.lb_controller.arn
  depends_on      = [aws_eks_addon.pod_identity]
}

# --- Platform backups (RFC-0037) ---------------------------------------------
# The server's service account (the backup CronJob runs under it) reads,
# writes and prunes archives in the backup bucket, nothing else.

data "aws_iam_policy_document" "backup" {
  count = var.backup_bucket != "" ? 1 : 0

  statement {
    actions   = ["s3:ListBucket", "s3:GetBucketLocation"]
    resources = ["arn:aws:s3:::${var.backup_bucket}"]
  }
  statement {
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    resources = ["arn:aws:s3:::${var.backup_bucket}/*"]
  }
}

resource "aws_iam_role" "backup" {
  count = var.backup_bucket != "" ? 1 : 0

  name               = "${var.name}-platform-backup"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_assume.json
}

resource "aws_iam_role_policy" "backup" {
  count = var.backup_bucket != "" ? 1 : 0

  name   = "s3-${var.backup_bucket}"
  role   = aws_iam_role.backup[0].name
  policy = data.aws_iam_policy_document.backup[0].json
}

resource "aws_eks_pod_identity_association" "backup" {
  count = var.backup_bucket != "" ? 1 : 0

  cluster_name    = aws_eks_cluster.this.name
  namespace       = "shpyrd-system"
  service_account = "shpyrd-server"
  role_arn        = aws_iam_role.backup[0].arn
  depends_on      = [aws_eks_addon.pod_identity]
}
