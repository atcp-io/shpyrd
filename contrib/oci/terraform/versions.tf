terraform {
  # Works with Terraform 1.5+ (the last MPL release) and OpenTofu.
  required_version = ">= 1.5"

  required_providers {
    oci = {
      source  = "oracle/oci"
      version = "~> 9.0"
    }
    http = {
      source  = "hashicorp/http"
      version = "~> 3.4"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
    local = {
      source  = "hashicorp/local"
      version = "~> 2.5"
    }
  }
}

# Authenticates with ~/.oci/config (the same profile the oci CLI uses).
provider "oci" {
  region              = var.region
  config_file_profile = var.config_file_profile
}
