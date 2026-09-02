output "secondary_authority_id" {
  description = "Vultr instance ID for the independent authority."
  value       = vultr_instance.secondary_authority.id
}

output "secondary_authority_ipv4" {
  description = "IPv4 address to verify directly before delegation."
  value       = vultr_instance.secondary_authority.main_ip
}

output "secondary_authority_ipv6" {
  description = "IPv6 address to verify directly before delegation."
  value       = vultr_instance.secondary_authority.v6_main_ip
}

output "snapshot_url" {
  description = "Authenticated snapshot endpoint configured on the secondary authority."
  value       = local.snapshot_url
}

output "control_url" {
  description = "Authenticated Simple DNS operator UI and Connect API."
  value       = "https://${local.control_hostname}"
}

output "authority_names" {
  description = "Nameservers placed into managed NS/SOA data."
  value       = [local.primary_nameserver, local.secondary_nameserver]
}
