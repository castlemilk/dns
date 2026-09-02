variable "cloudflare_zone_id" {
  description = "Cloudflare zone ID that continues to host the nameserver address records and canary delegation."
  type        = string
}

variable "primary_nameserver" {
  description = "FQDN of the VKE-backed authority, without relying on registrar glue for the canary zone."
  type        = string
}

variable "secondary_nameserver" {
  description = "FQDN of the independently hosted Melbourne authority."
  type        = string
}

variable "primary_authority_ipv4" {
  description = "Stable IPv4 address produced by the separately reconciled VKE DNS load balancer."
  type        = string

  validation {
    condition     = can(cidrhost("${var.primary_authority_ipv4}/32", 0))
    error_message = "primary_authority_ipv4 must be a valid IPv4 address."
  }
}

variable "primary_authority_ipv6" {
  description = "Optional verified IPv6 address from the VKE DNS load balancer. Leave null until direct UDP and TCP IPv6 probes pass."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.primary_authority_ipv6 == null || (can(cidrhost("${var.primary_authority_ipv6}/128", 0)) && can(regex(":", var.primary_authority_ipv6)))
    error_message = "primary_authority_ipv6 must be null or a valid IPv6 address."
  }
}

variable "snapshot_hostname" {
  description = "FQDN routed through the Paprika Gateway to only /internal/v1/snapshot."
  type        = string
}

variable "control_hostname" {
  description = "FQDN for the authenticated operator UI and Connect API through the Paprika Gateway."
  type        = string
}

variable "snapshot_gateway_ipv4" {
  description = "Paprika Gateway's stable public IPv4 address."
  type        = string

  validation {
    condition     = can(cidrhost("${var.snapshot_gateway_ipv4}/32", 0))
    error_message = "snapshot_gateway_ipv4 must be a valid IPv4 address."
  }
}

variable "canary_zone" {
  description = "Child zone delegated from Cloudflare only after both authorities pass external UDP and TCP checks."
  type        = string
}

variable "enable_canary_delegation" {
  description = "Create the NS delegation only after the runbook's direct UDP/TCP and recovery gates pass."
  type        = bool
  default     = false
}

variable "secondary_region" {
  description = "Independent Vultr region for the second authority."
  type        = string
  default     = "mel"
}

variable "secondary_plan" {
  description = "Small compute plan for the read-only authority."
  type        = string
  default     = "vc2-1c-1gb"
}

variable "secondary_os_id" {
  description = "Vultr OS ID. 2284 is Ubuntu 24.04 LTS x64 in the 2026-09-02 catalog."
  type        = number
  default     = 2284
}

variable "secondary_ssh_key_ids" {
  description = "Vultr SSH public-key IDs installed on the secondary authority."
  type        = set(string)

  validation {
    condition     = length(var.secondary_ssh_key_ids) > 0
    error_message = "At least one SSH key ID is required."
  }
}

variable "operator_ipv4_cidr" {
  description = "Single trusted operator IPv4 CIDR allowed to SSH, for example 203.0.113.8/32."
  type        = string

  validation {
    condition     = can(cidrhost(var.operator_ipv4_cidr, 0)) && can(regex("^[0-9.]+/[0-9]+$", var.operator_ipv4_cidr))
    error_message = "operator_ipv4_cidr must be a valid IPv4 CIDR."
  }
}

variable "operator_ipv6_cidr" {
  description = "Optional trusted operator IPv6 CIDR allowed to SSH."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.operator_ipv6_cidr == null || (can(cidrhost(var.operator_ipv6_cidr, 0)) && can(regex(":", var.operator_ipv6_cidr)))
    error_message = "operator_ipv6_cidr must be null or a valid IPv6 CIDR."
  }
}

variable "server_image" {
  description = "Public, digest-pinned linux/amd64 authority image."
  type        = string

  validation {
    condition     = can(regex("^ghcr\\.io/castlemilk/dns@sha256:[0-9a-f]{64}$", var.server_image))
    error_message = "server_image must be ghcr.io/castlemilk/dns@sha256:<64 lowercase hex characters>."
  }
}

variable "enable_enhanced_ddos_protection" {
  description = "Enable Vultr's optional 10 Gbps DDoS add-on on the secondary authority (currently an additional monthly charge)."
  type        = bool
  default     = false
}

variable "publish_secondary_ipv6" {
  description = "Publish the Melbourne authority AAAA record only after direct UDP and TCP IPv6 probes pass."
  type        = bool
  default     = false
}
