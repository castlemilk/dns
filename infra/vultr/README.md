# Vultr secondary authority and delegation edge

This OpenTofu stack creates the failure domain outside the Sydney VKE cluster:

- a small Melbourne read-only authority with a restricted Vultr firewall
  permitting DNS, operator SSH, and the ICMP/ICMPv6 needed for diagnostics and
  path-MTU discovery;
- unproxied Cloudflare A/AAAA records for both nameservers plus a proxied
  operator hostname;
- an unproxied snapshot hostname pointing at the Paprika Gateway; and
- an opt-in child-zone NS delegation, disabled by default.

The Sydney mixed-protocol load balancer is reconciled separately by
`cmd/vultr-dns-lb`. The current Vultr Terraform provider rejects UDP in its
load-balancer schema even though the Vultr v2 API supports UDP forwarding, so
this stack does not pretend to manage that endpoint partially.

Read [`../../docs/production-launch.md`](../../docs/production-launch.md) before
planning or applying.

## Initialize and plan

Use a scoped Cloudflare API token and a dedicated Vultr key supplied by the
approved secret manager:

```sh
export CLOUDFLARE_API_TOKEN='<injected>'
export VULTR_API_KEY='<injected>'

tofu init -backend-config=backend.hcl
tofu plan -var-file=production.tfvars
```

`backend.hcl` and `production.tfvars` are deliberately ignored. Start from the
example files, store state remotely, and do not put either provider credential
in a tfvars file. Review the estimated monthly charges in Vultr before apply.

Keep `enable_canary_delegation = false` for the initial apply. After the VKE
endpoint and Melbourne endpoint pass the full direct UDP/TCP and recovery
preflight, change only that value, review the two NS-record additions, and apply
again.

IPv6 address records are also guarded. Leave `primary_authority_ipv6 = null`
and `publish_secondary_ipv6 = false` until each address passes direct UDP and
TCP probes; a reachable-looking but broken AAAA record can make otherwise
healthy delegations intermittently fail for IPv6 resolvers.

## Credential bootstrap

The VM cannot start the authority until `/etc/simpledns/authority.env` exists.
Inject only the snapshot token over SSH stdin:

```sh
SECONDARY_AUTHORITY_IP="$(tofu output -raw secondary_authority_ipv4)" \
  ./bootstrap-authority.sh
```

The script prompts without echo unless `DNS_SNAPSHOT_BEARER_TOKEN` is already
in the environment. It also requires the instance's ED25519 SSH host-key
fingerprint in `SECONDARY_AUTHORITY_HOST_KEY_SHA256` (or prompts for it).
Obtain that fingerprint through the authenticated Vultr web console with
`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256`; do not trust a
fingerprint learned from the same network connection. The helper uses a
temporary pinned `known_hosts` file and never stores the token in OpenTofu
state or a local temporary file.
