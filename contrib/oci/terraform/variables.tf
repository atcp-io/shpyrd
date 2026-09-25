# Identity and placement

variable "tenancy_ocid" {
  description = "Tenancy OCID (oci iam availability-domain list shows it as compartment-id)."
  type        = string
}

variable "compartment_ocid" {
  description = "Compartment for every resource; the tenancy root when empty (fine for a development cluster; production wants its own compartment)."
  type        = string
  default     = ""
}

variable "region" {
  description = "OCI region identifier, for example sa-saopaulo-1."
  type        = string
}

variable "config_file_profile" {
  description = "Profile in ~/.oci/config used to authenticate."
  type        = string
  default     = "DEFAULT"
}

variable "name" {
  description = "Name of the platform; prefixes every resource and names the cluster."
  type        = string
  default     = "shpyrd-dev"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}$", var.name))
    error_message = "Lowercase letters, digits and dashes, starting with a letter."
  }
}

# Cluster

variable "kubernetes_version" {
  description = "OKE Kubernetes version (oci ce cluster-options get --cluster-option-id all lists them)."
  type        = string
  default     = "v1.36.1"
}

variable "cluster_type" {
  description = "BASIC_CLUSTER is free; ENHANCED_CLUSTER adds workload identity, add-on management and an SLA for a per-cluster fee. Basic upgrades to enhanced in place."
  type        = string
  default     = "BASIC_CLUSTER"

  validation {
    condition     = contains(["BASIC_CLUSTER", "ENHANCED_CLUSTER"], var.cluster_type)
    error_message = "BASIC_CLUSTER or ENHANCED_CLUSTER."
  }
}

variable "services_cidr" {
  description = "Kubernetes Service (ClusterIP) range."
  type        = string
  default     = "10.96.0.0/16"
}

# Workers

variable "node_shape" {
  description = "Worker shape. VM.Standard.A1.Flex (Ampere, Always Free) when the region has capacity; VM.Standard.E5.Flex (AMD) otherwise."
  type        = string
  default     = "VM.Standard.E5.Flex"
}

variable "node_ocpus" {
  type    = number
  default = 2
}

variable "node_memory_gb" {
  type    = number
  default = 12
}

variable "node_count" {
  type    = number
  default = 2
}

variable "node_boot_volume_gb" {
  type    = number
  default = 100
}

variable "max_pods_per_node" {
  description = "VCN-native pod networking: pods per node is bounded by the shape's VNICs (31 on a 2-OCPU shape)."
  type        = number
  default     = 31
}

variable "ssh_public_key_path" {
  description = "Public key installed on the workers (ssh through the Bastion service)."
  type        = string
  default     = "~/.ssh/id_ed25519.pub"
}

# Access

variable "admin_cidrs" {
  description = "Who may open Bastion sessions to the Kubernetes API. Empty: this machine's public address."
  type        = list(string)
  default     = []
}

# Network layout (RFC-0035). The pod subnet is a /22 on a multiple of four.

variable "vcn_cidr" {
  type    = string
  default = "10.0.0.0/16"
}

variable "subnet_cidrs" {
  description = "Subnets inside vcn_cidr."
  type = object({
    api        = string                           # Kubernetes API endpoint (private)
    workers    = string                           # worker nodes (private)
    pods       = string                           # pods, VCN-native networking (private)
    lb_public  = string                           # internet-facing load balancers
    lb_private = string                           # internal load balancers (RFC-0036)
    bastion    = string                           # OCI Bastion service
    vpn        = optional(string, "10.0.30.0/24") # the WireGuard instance (public)
  })
  default = {
    api        = "10.0.0.0/24"
    workers    = "10.0.1.0/24"
    pods       = "10.0.4.0/22"
    lb_public  = "10.0.10.0/24"
    lb_private = "10.0.11.0/24"
    bastion    = "10.0.20.0/24"
    vpn        = "10.0.30.0/24"
  }
}

# Public address and DNS

variable "reserved_public_ip" {
  description = "Reserve a public address for the external load balancer so it survives cluster rebuilds (pass it to shpyrd as SHPYRD_LB_IP)."
  type        = bool
  default     = true
}

variable "dns_zone" {
  description = "Create this public zone in OCI DNS with a wildcard record for the platform (for example oci.example.com; delegate it from the parent zone with the NS records in the outputs). Empty: no zone."
  type        = string
  default     = ""
}
