# Where the platform's backups go (RFC-0037): an S3 bucket of its own, apart
# from the cluster's root, so `terraform destroy` over there leaves the
# archives here and a new cluster restores from them. The cluster root grants
# the platform access to it (backup_bucket) through EKS Pod Identity: no keys.
#
#   terraform init && terraform apply
#   # then in ../terraform.tfvars:  backup_bucket = "<bucket>"

terraform {
  required_version = ">= 1.5"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}

provider "aws" {
  region  = var.region
  profile = var.profile != "" ? var.profile : null

  default_tags {
    tags = {
      "shpyrd.io/platform" = var.name
      "ManagedBy"          = "terraform"
    }
  }
}

variable "name" {
  description = "Name of the platform (the cluster root's name variable)."
  type        = string
  default     = "shpyrd-dev"
}

variable "region" {
  description = "Region of the bucket; the cluster's region keeps transfers in-region."
  type        = string
  default     = "us-east-1"
}

variable "profile" {
  description = "Named profile in ~/.aws/credentials; empty uses the environment."
  type        = string
  default     = ""
}

data "aws_caller_identity" "current" {}

# Bucket names are global: the account id keeps it unique.
resource "aws_s3_bucket" "backups" {
  bucket = "${var.name}-backups-${data.aws_caller_identity.current.account_id}"
}

resource "aws_s3_bucket_public_access_block" "backups" {
  bucket                  = aws_s3_bucket.backups.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "backups" {
  bucket = aws_s3_bucket.backups.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

output "bucket" {
  value = aws_s3_bucket.backups.bucket
}

output "target" {
  description = "Value for --backup-target."
  value       = "s3://${aws_s3_bucket.backups.bucket}/${var.name}"
}

output "next_steps" {
  value = <<-EOT
    # in ../terraform.tfvars, then terraform apply over there (Pod Identity grant + the vars file gains the target):
    backup_bucket = "${aws_s3_bucket.backups.bucket}"
  EOT
}
