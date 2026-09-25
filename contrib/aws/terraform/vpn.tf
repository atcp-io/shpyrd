# Access path into the VPC: AWS Client VPN with certificate authentication.
# Terraform is the certificate authority: it generates the CA, the server
# certificate (imported to ACM) and one client certificate, and writes the
# profile the AWS VPN Client imports. Connected, this machine reaches the
# private front door, the private API endpoint and internal projects;
# nothing else is exposed.
#
# Cost: the subnet association is billed per hour while it exists, each
# connection per hour while connected (vpn = false removes it).

resource "tls_private_key" "vpn_ca" {
  count     = var.vpn ? 1 : 0
  algorithm = "RSA"
  rsa_bits  = 2048
}

resource "tls_self_signed_cert" "vpn_ca" {
  count = var.vpn ? 1 : 0

  private_key_pem       = tls_private_key.vpn_ca[0].private_key_pem
  is_ca_certificate     = true
  validity_period_hours = 10 * 365 * 24
  allowed_uses          = ["cert_signing", "crl_signing", "digital_signature"]
  subject {
    common_name  = "${var.name} vpn ca"
    organization = var.name
  }
}

resource "tls_private_key" "vpn_server" {
  count     = var.vpn ? 1 : 0
  algorithm = "RSA"
  rsa_bits  = 2048
}

# ACM wants a domain name as the subject (it need not resolve anywhere).
locals {
  vpn_server_name = "vpn.${var.name}.internal"
}

resource "tls_cert_request" "vpn_server" {
  count           = var.vpn ? 1 : 0
  private_key_pem = tls_private_key.vpn_server[0].private_key_pem
  dns_names       = [local.vpn_server_name]
  subject {
    common_name  = local.vpn_server_name
    organization = var.name
  }
}

resource "tls_locally_signed_cert" "vpn_server" {
  count = var.vpn ? 1 : 0

  cert_request_pem      = tls_cert_request.vpn_server[0].cert_request_pem
  ca_private_key_pem    = tls_private_key.vpn_ca[0].private_key_pem
  ca_cert_pem           = tls_self_signed_cert.vpn_ca[0].cert_pem
  validity_period_hours = 5 * 365 * 24
  allowed_uses          = ["key_encipherment", "digital_signature", "server_auth"]
}

resource "tls_private_key" "vpn_client" {
  count     = var.vpn ? 1 : 0
  algorithm = "RSA"
  rsa_bits  = 2048
}

resource "tls_cert_request" "vpn_client" {
  count           = var.vpn ? 1 : 0
  private_key_pem = tls_private_key.vpn_client[0].private_key_pem
  subject {
    common_name  = "${var.name}-vpn-client"
    organization = var.name
  }
}

resource "tls_locally_signed_cert" "vpn_client" {
  count = var.vpn ? 1 : 0

  cert_request_pem      = tls_cert_request.vpn_client[0].cert_request_pem
  ca_private_key_pem    = tls_private_key.vpn_ca[0].private_key_pem
  ca_cert_pem           = tls_self_signed_cert.vpn_ca[0].cert_pem
  validity_period_hours = 5 * 365 * 24
  allowed_uses          = ["key_encipherment", "digital_signature", "client_auth"]
}

# The server certificate; the same CA signed the client certificate, so it
# also serves as the client root chain (AWS documents that shortcut).
resource "aws_acm_certificate" "vpn" {
  count = var.vpn ? 1 : 0

  private_key       = tls_private_key.vpn_server[0].private_key_pem
  certificate_body  = tls_locally_signed_cert.vpn_server[0].cert_pem
  certificate_chain = tls_self_signed_cert.vpn_ca[0].cert_pem
  tags              = { Name = "${var.name}-vpn" }
}

resource "aws_security_group" "vpn" {
  count = var.vpn ? 1 : 0

  name        = "${var.name}-vpn"
  description = "Client VPN endpoint: clients reach the VPC"
  vpc_id      = aws_vpc.this.id
  tags        = { Name = "${var.name}-vpn" }
}

resource "aws_vpc_security_group_egress_rule" "vpn_to_vpc" {
  count = var.vpn ? 1 : 0

  security_group_id = aws_security_group.vpn[0].id
  cidr_ipv4         = var.vpc_cidr
  ip_protocol       = "-1"
  description       = "VPN clients to the VPC"
}

resource "aws_ec2_client_vpn_endpoint" "this" {
  count = var.vpn ? 1 : 0

  description            = "${var.name}: access to the platform's private front door and API"
  server_certificate_arn = aws_acm_certificate.vpn[0].arn
  client_cidr_block      = var.vpn_client_cidr
  split_tunnel           = true
  vpc_id                 = aws_vpc.this.id
  security_group_ids     = [aws_security_group.vpn[0].id]
  # The VPC resolver, so the cluster's private API endpoint resolves while
  # connected; it answers public names too.
  dns_servers           = [local.vpc_resolver]
  transport_protocol    = "udp"
  vpn_port              = 443
  session_timeout_hours = 24

  authentication_options {
    type                       = "certificate-authentication"
    root_certificate_chain_arn = aws_acm_certificate.vpn[0].arn
  }

  connection_log_options {
    enabled = false
  }

  tags = { Name = "${var.name}-vpn" }
}

resource "aws_ec2_client_vpn_network_association" "this" {
  count = var.vpn ? 1 : 0

  client_vpn_endpoint_id = aws_ec2_client_vpn_endpoint.this[0].id
  subnet_id              = aws_subnet.private[0].id
}

resource "aws_ec2_client_vpn_authorization_rule" "vpc" {
  count = var.vpn ? 1 : 0

  client_vpn_endpoint_id = aws_ec2_client_vpn_endpoint.this[0].id
  target_network_cidr    = var.vpc_cidr
  authorize_all_groups   = true
}

# The profile for the AWS VPN Client (File > Manage Profiles > Add Profile),
# the same shape as the one the console exports, with the client certificate
# and key inlined. It is a credential: keep it with the state.
resource "local_sensitive_file" "vpn_profile" {
  count = var.vpn ? 1 : 0

  filename        = "${path.module}/${var.name}-vpn.ovpn"
  file_permission = "0600"
  content         = <<-EOT
    client
    dev tun
    proto udp
    remote ${trimprefix(aws_ec2_client_vpn_endpoint.this[0].dns_name, "*.")} 443
    remote-random-hostname
    resolv-retry infinite
    nobind
    remote-cert-tls server
    cipher AES-256-GCM
    verb 3
    <ca>
    ${chomp(tls_self_signed_cert.vpn_ca[0].cert_pem)}
    </ca>
    <cert>
    ${chomp(tls_locally_signed_cert.vpn_client[0].cert_pem)}
    </cert>
    <key>
    ${chomp(tls_private_key.vpn_client[0].private_key_pem)}
    </key>

    reneg-sec 0

    verify-x509-name ${local.vpn_server_name} name
  EOT
}
