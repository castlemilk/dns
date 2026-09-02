terraform {
  required_version = ">= 1.8.0"

  # Production initialization must supply a remote S3-compatible backend
  # configuration. Keep state, which contains infrastructure metadata, out of
  # developer workstations and this repository.
  backend "s3" {}

  required_providers {
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.23.0"
    }
    vultr = {
      source  = "vultr/vultr"
      version = "~> 2.32.0"
    }
  }
}

provider "cloudflare" {}

provider "vultr" {}
