terraform {
  # Works with Terraform 1.5+ (the last MPL release) and OpenTofu.
  required_version = ">= 1.5"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
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

# Authenticates like the aws CLI: a named profile when given, else the
# environment (AWS_PROFILE, AWS_ACCESS_KEY_ID, an SSO session).
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
