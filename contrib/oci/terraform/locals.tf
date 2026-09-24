data "oci_identity_availability_domains" "ads" {
  compartment_id = var.tenancy_ocid
}

# "All <region> Services In Oracle Services Network": the service gateway
# target and the destination of the OCI-services egress rules.
data "oci_core_services" "all" {
  filter {
    name   = "name"
    values = ["All .* Services In Oracle Services Network"]
    regex  = true
  }
}

# This machine's public address, for the Bastion allow list when admin_cidrs is empty.
data "http" "my_ip" {
  count = length(var.admin_cidrs) == 0 ? 1 : 0
  url   = "https://api.ipify.org"
}

locals {
  compartment_id = var.compartment_ocid != "" ? var.compartment_ocid : var.tenancy_ocid
  ad             = data.oci_identity_availability_domains.ads.availability_domains[0].name
  services_id    = data.oci_core_services.all.services[0].id
  services_cidr  = data.oci_core_services.all.services[0].cidr_block
  admin_cidrs    = length(var.admin_cidrs) > 0 ? var.admin_cidrs : ["${trimspace(data.http.my_ip[0].response_body)}/32"]
  ssh_public_key = trimspace(file(pathexpand(var.ssh_public_key_path)))
  # OKE names must be alphanumeric for the Bastion; DNS labels are letters and digits.
  bastion_name = replace(var.name, "-", "")
  dns_label    = substr(replace(var.name, "-", ""), 0, 15)
  cidr         = var.subnet_cidrs
}
