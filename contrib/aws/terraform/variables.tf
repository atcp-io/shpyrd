variable "profile" {
  description = "AWS CLI profile to authenticate with; empty uses the environment."
  type        = string
  default     = ""
}

variable "region" {
  description = "Region for every resource."
  type        = string
  default     = "us-east-1"
}

variable "name" {
  description = "Name of the platform; prefixes every resource and names the cluster."
  type        = string
  default     = "shpyrd-dev"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}$", var.name))
    error_message = "Lowercase letters, digits and dashes, starting with a letter, at most 31 characters."
  }
}

variable "kubernetes_version" {
  description = "EKS Kubernetes version."
  type        = string
  default     = "1.36"
}

variable "vpc_cidr" {
  description = "VPC range; pods take addresses from it (VPC CNI)."
  type        = string
  default     = "10.0.0.0/16"
}

variable "service_cidr" {
  description = "Kubernetes Service range; the in-cluster registry sits at .0.50 of it."
  type        = string
  default     = "10.100.0.0/16"
}

variable "node_instance_type" {
  description = "Instance type of the managed node group."
  type        = string
  default     = "t3a.large"
}

variable "node_count" {
  description = "Nodes in the managed node group."
  type        = number
  default     = 2
}

variable "node_disk_gb" {
  description = "Root disk of each node, GB (image layers of builds live here)."
  type        = number
  default     = 60
}

variable "api_public_access" {
  description = "Also expose the Kubernetes API on a public endpoint (restricted to admin_cidrs). Off by default: the API is reachable from inside the VPC only, through the VPN."
  type        = bool
  default     = false
}

variable "nat_gateway_per_az" {
  description = "One NAT gateway per availability zone (egress survives a zone outage); false shares a single one, the cheaper layout for a development cluster."
  type        = bool
  default     = true
}

variable "admin_cidrs" {
  description = "Addresses allowed to reach the public Kubernetes API endpoint when api_public_access is on; empty means this machine's address."
  type        = list(string)
  default     = []
}

variable "dns_zone" {
  description = "Public zone in Route 53 for the platform (delegate it from its parent); empty for none."
  type        = string
  default     = ""
}

variable "vpn" {
  description = "Create an AWS Client VPN endpoint into the VPC and write <name>-vpn.ovpn for the AWS VPN Client. Billed per hour while the subnet association exists."
  type        = bool
  default     = true
}

variable "vpn_client_cidr" {
  description = "Address range handed to VPN clients (at least /22, must not overlap the VPC)."
  type        = string
  default     = "10.8.0.0/22"
}

variable "shared_storage" {
  description = "Create an EFS file system for shared (ReadWriteMany) volumes (RFC-0060); billed by the space used."
  type        = bool
  default     = true
}
