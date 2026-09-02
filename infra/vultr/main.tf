locals {
  primary_nameserver   = trimsuffix(lower(var.primary_nameserver), ".")
  secondary_nameserver = trimsuffix(lower(var.secondary_nameserver), ".")
  control_hostname     = trimsuffix(lower(var.control_hostname), ".")
  snapshot_hostname    = trimsuffix(lower(var.snapshot_hostname), ".")
  canary_zone          = trimsuffix(lower(var.canary_zone), ".")
  snapshot_url         = "https://${local.snapshot_hostname}/internal/v1/snapshot"
}

resource "vultr_firewall_group" "secondary_authority" {
  description = "simpledns secondary authority"
}

resource "vultr_firewall_rule" "dns_tcp_v4" {
  firewall_group_id = vultr_firewall_group.secondary_authority.id
  protocol          = "tcp"
  ip_type           = "v4"
  subnet            = "0.0.0.0"
  subnet_size       = 0
  port              = "53"
  notes             = "Public authoritative DNS over TCP"
}

resource "vultr_firewall_rule" "dns_udp_v4" {
  firewall_group_id = vultr_firewall_group.secondary_authority.id
  protocol          = "udp"
  ip_type           = "v4"
  subnet            = "0.0.0.0"
  subnet_size       = 0
  port              = "53"
  notes             = "Public authoritative DNS over UDP"
}

resource "vultr_firewall_rule" "dns_tcp_v6" {
  firewall_group_id = vultr_firewall_group.secondary_authority.id
  protocol          = "tcp"
  ip_type           = "v6"
  subnet            = "::"
  subnet_size       = 0
  port              = "53"
  notes             = "Public authoritative DNS over TCP"
}

resource "vultr_firewall_rule" "dns_udp_v6" {
  firewall_group_id = vultr_firewall_group.secondary_authority.id
  protocol          = "udp"
  ip_type           = "v6"
  subnet            = "::"
  subnet_size       = 0
  port              = "53"
  notes             = "Public authoritative DNS over UDP"
}

# Preserve path-MTU discovery and IPv6 neighbor/error signalling. DNSSEC-sized
# UDP replies and TCP fallback are unreliable when required ICMP is dropped.
resource "vultr_firewall_rule" "icmp_v4" {
  firewall_group_id = vultr_firewall_group.secondary_authority.id
  protocol          = "icmp"
  ip_type           = "v4"
  subnet            = "0.0.0.0"
  subnet_size       = 0
  notes             = "Public ICMP for diagnostics and path MTU discovery"
}

resource "vultr_firewall_rule" "icmp_v6" {
  firewall_group_id = vultr_firewall_group.secondary_authority.id
  protocol          = "icmp"
  ip_type           = "v6"
  subnet            = "::"
  subnet_size       = 0
  notes             = "Public ICMPv6 for network operation and path MTU discovery"
}

resource "vultr_firewall_rule" "operator_ssh_v4" {
  firewall_group_id = vultr_firewall_group.secondary_authority.id
  protocol          = "tcp"
  ip_type           = "v4"
  subnet            = split("/", var.operator_ipv4_cidr)[0]
  subnet_size       = tonumber(split("/", var.operator_ipv4_cidr)[1])
  port              = "22"
  notes             = "Restricted operator SSH"
}

resource "vultr_firewall_rule" "operator_ssh_v6" {
  count = var.operator_ipv6_cidr == null ? 0 : 1

  firewall_group_id = vultr_firewall_group.secondary_authority.id
  protocol          = "tcp"
  ip_type           = "v6"
  subnet            = split("/", var.operator_ipv6_cidr)[0]
  subnet_size       = tonumber(split("/", var.operator_ipv6_cidr)[1])
  port              = "22"
  notes             = "Restricted operator SSH"
}

resource "vultr_instance" "secondary_authority" {
  region            = var.secondary_region
  plan              = var.secondary_plan
  os_id             = var.secondary_os_id
  label             = "simpledns-authority-mel"
  hostname          = "simpledns-authority-mel"
  tags              = ["simpledns", "authoritative-dns", "production"]
  ssh_key_ids       = sort(tolist(var.secondary_ssh_key_ids))
  firewall_group_id = vultr_firewall_group.secondary_authority.id
  enable_ipv6       = true
  ddos_protection   = var.enable_enhanced_ddos_protection
  backups           = "disabled"
  activation_email  = false
  user_scheme       = "root"

  user_data = templatefile("${path.module}/secondary-authority.cloud-init.yaml.tftpl", {
    image_ref          = var.server_image
    snapshot_url       = local.snapshot_url
    primary_nameserver = local.primary_nameserver
    second_nameserver  = local.secondary_nameserver
    canary_zone        = local.canary_zone
  })

  lifecycle {
    prevent_destroy = true
  }
}

resource "cloudflare_dns_record" "primary_nameserver_v4" {
  zone_id = var.cloudflare_zone_id
  name    = local.primary_nameserver
  type    = "A"
  content = var.primary_authority_ipv4
  ttl     = 300
  proxied = false
  comment = "simpledns VKE authority; must never be proxied"
}

resource "cloudflare_dns_record" "primary_nameserver_v6" {
  count = var.primary_authority_ipv6 == null ? 0 : 1

  zone_id = var.cloudflare_zone_id
  name    = local.primary_nameserver
  type    = "AAAA"
  content = var.primary_authority_ipv6
  ttl     = 300
  proxied = false
  comment = "Verified simpledns VKE authority IPv6; must never be proxied"
}

resource "cloudflare_dns_record" "secondary_nameserver_v4" {
  zone_id = var.cloudflare_zone_id
  name    = local.secondary_nameserver
  type    = "A"
  content = vultr_instance.secondary_authority.main_ip
  ttl     = 300
  proxied = false
  comment = "simpledns Melbourne authority; must never be proxied"
}

resource "cloudflare_dns_record" "secondary_nameserver_v6" {
  count = var.publish_secondary_ipv6 ? 1 : 0

  zone_id = var.cloudflare_zone_id
  name    = local.secondary_nameserver
  type    = "AAAA"
  content = vultr_instance.secondary_authority.v6_main_ip
  ttl     = 300
  proxied = false
  comment = "simpledns Melbourne authority IPv6; must never be proxied"
}

resource "cloudflare_dns_record" "snapshot" {
  zone_id = var.cloudflare_zone_id
  name    = local.snapshot_hostname
  type    = "A"
  content = var.snapshot_gateway_ipv4
  ttl     = 300
  proxied = false
  comment = "Authenticated simpledns snapshot feed through the Paprika Gateway"
}

resource "cloudflare_dns_record" "control" {
  zone_id = var.cloudflare_zone_id
  name    = local.control_hostname
  type    = "A"
  content = var.snapshot_gateway_ipv4
  ttl     = 1
  proxied = true
  comment = "Authenticated simpledns operator UI and Connect API through Cloudflare to the Paprika Gateway"
}

# Apply the child delegation only after both authorities have independently
# passed direct UDP and TCP canaries. The explicit dependency prevents it from
# racing ahead of the secondary instance during an initial apply, but operators
# still gate the apply with the runbook preflight.
resource "cloudflare_dns_record" "canary_ns_primary" {
  count = var.enable_canary_delegation ? 1 : 0

  zone_id = var.cloudflare_zone_id
  name    = local.canary_zone
  type    = "NS"
  content = local.primary_nameserver
  ttl     = 300
  proxied = false

  depends_on = [
    cloudflare_dns_record.primary_nameserver_v4,
    cloudflare_dns_record.secondary_nameserver_v4,
  ]
}

resource "cloudflare_dns_record" "canary_ns_secondary" {
  count = var.enable_canary_delegation ? 1 : 0

  zone_id = var.cloudflare_zone_id
  name    = local.canary_zone
  type    = "NS"
  content = local.secondary_nameserver
  ttl     = 300
  proxied = false

  depends_on = [
    cloudflare_dns_record.primary_nameserver_v4,
    cloudflare_dns_record.secondary_nameserver_v4,
  ]
}
