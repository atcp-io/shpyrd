# Where the platform's backups go (RFC-0037): an Object Storage bucket and
# an IAM user whose Customer Secret Key signs S3-compatible requests to it.
# A root of its own, apart from the cluster's: `terraform destroy` over there
# leaves the archives here, and a new cluster restores from them.
#
#   terraform init && terraform apply
#   # then in ../terraform.tfvars:  backup_bucket = "<name>-backups"
#
# The bucket lives in the tenancy's designated S3-compatibility compartment
# (the root compartment unless changed): the S3 API only sees buckets there.

terraform {
  required_version = ">= 1.5"

  required_providers {
    oci = {
      source  = "oracle/oci"
      version = "~> 9.0"
    }
    local = {
      source  = "hashicorp/local"
      version = "~> 2.5"
    }
  }
}

provider "oci" {
  region              = var.region
  config_file_profile = var.config_file_profile
}

# IAM lives in the home region.
provider "oci" {
  alias               = "home"
  region              = local.home_region
  config_file_profile = var.config_file_profile
}

variable "tenancy_ocid" {
  description = "Tenancy OCID."
  type        = string
}

variable "region" {
  description = "Region of the bucket; the cluster's region keeps transfers in-region."
  type        = string
}

variable "config_file_profile" {
  description = "Profile in ~/.oci/config used to authenticate."
  type        = string
  default     = "DEFAULT"
}

variable "name" {
  description = "Name of the platform (the cluster root's name variable); the bucket is <name>-backups."
  type        = string
  default     = "shpyrd-dev"
}

data "oci_identity_tenancy" "this" {
  tenancy_id = var.tenancy_ocid
}

data "oci_identity_regions" "all" {}

data "oci_objectstorage_namespace" "this" {
  compartment_id = var.tenancy_ocid
}

data "oci_objectstorage_namespace_metadata" "this" {
  namespace = data.oci_objectstorage_namespace.this.namespace
}

locals {
  home_region = [for r in data.oci_identity_regions.all.regions : r.name if r.key == data.oci_identity_tenancy.this.home_region_key][0]
  namespace   = data.oci_objectstorage_namespace.this.namespace
  # Buckets the S3 API can see live in the namespace's designated
  # compartment (the root compartment by default).
  s3_compartment = coalesce(data.oci_objectstorage_namespace_metadata.this.default_s3compartment_id, var.tenancy_ocid)
  bucket         = "${var.name}-backups"
  endpoint       = "https://${local.namespace}.compat.objectstorage.${var.region}.oraclecloud.com"
}

resource "oci_objectstorage_bucket" "backups" {
  compartment_id = local.s3_compartment
  namespace      = local.namespace
  name           = local.bucket
  access_type    = "NoPublicAccess"
  storage_tier   = "Standard"
  versioning     = "Disabled"

  freeform_tags = { "shpyrd-platform" = var.name }
}

resource "oci_identity_group" "backup" {
  provider = oci.home

  compartment_id = var.tenancy_ocid
  name           = "${var.name}-backup"
  description    = "Platform backups of ${var.name}: objects in bucket ${local.bucket} only"
}

resource "oci_identity_user" "backup" {
  provider = oci.home

  compartment_id = var.tenancy_ocid
  name           = "${var.name}-backup"
  description    = "Platform backups of ${var.name}; Customer Secret Key only, no console access"
  email          = "${var.name}-backup@${var.name}.invalid"
}

resource "oci_identity_user_group_membership" "backup" {
  provider = oci.home

  group_id = oci_identity_group.backup.id
  user_id  = oci_identity_user.backup.id
}

# The S3-compatible access key: id is the access key, key the secret.
resource "oci_identity_customer_secret_key" "backup" {
  provider = oci.home

  user_id      = oci_identity_user.backup.id
  display_name = "${var.name}-backup"
}

resource "oci_identity_policy" "backup" {
  provider = oci.home

  compartment_id = var.tenancy_ocid
  name           = "${var.name}-backup"
  description    = "Let the ${var.name}-backup group read, write and delete objects in bucket ${local.bucket}"
  statements = [
    "Allow group ${oci_identity_group.backup.name} to read buckets in tenancy where target.bucket.name='${local.bucket}'",
    "Allow group ${oci_identity_group.backup.name} to manage objects in tenancy where target.bucket.name='${local.bucket}'",
  ]
}

# Credentials for `shpyrd cluster init --backup-credentials-file` and
# `shpyrd cluster restore --credentials-file`. A secret: git-ignored, 0600.
resource "local_sensitive_file" "credentials" {
  filename        = "${path.module}/${var.name}-backups.env"
  file_permission = "0600"
  content         = <<-EOT
    # Written by contrib/oci/terraform/backups: S3-compatible credentials for the ${local.bucket} bucket.
    AWS_ACCESS_KEY_ID=${oci_identity_customer_secret_key.backup.id}
    AWS_SECRET_ACCESS_KEY=${oci_identity_customer_secret_key.backup.key}
    SHPYRD_BACKUP_ENDPOINT=${local.endpoint}
    SHPYRD_BACKUP_REGION=${var.region}
  EOT
}

output "bucket" {
  value = oci_objectstorage_bucket.backups.name
}

output "target" {
  description = "Value for --backup-target."
  value       = "s3://${local.bucket}/${var.name}"
}

output "endpoint" {
  value = local.endpoint
}

output "credentials_file" {
  value = abspath(local_sensitive_file.credentials.filename)
}

output "next_steps" {
  value = <<-EOT
    # in ../terraform.tfvars, then terraform apply over there (the vars file gains the target):
    backup_bucket = "${local.bucket}"
    # shpyrd cluster init ... --backup-credentials-file ${abspath(local_sensitive_file.credentials.filename)}
    # (a new Customer Secret Key takes a few minutes to work: SignatureDoesNotMatch until then)
  EOT
}
