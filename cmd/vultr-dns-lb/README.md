# Vultr DNS load balancer reconciler

This command declaratively reconciles one Vultr Load Balancer in `syd` for the
authoritative DNS service. The default invocation is a read-only plan. Only
`--apply` permits a create or update.

The API key is accepted only through `VULTR_API_KEY`; there is no token flag.
Keep it out of shell history and never commit it.

## Plan with the four core VKE instances

Use the current four core worker instance IDs in place of these symbolic
values:

```sh
# First inject VULTR_API_KEY from a password manager or CI secret store.

go run ./cmd/vultr-dns-lb \
  --plan \
  --label='simpledns-authoritative-syd' \
  --instance-id='<core-vke-instance-id-1>' \
  --instance-id='<core-vke-instance-id-2>' \
  --instance-id='<core-vke-instance-id-3>' \
  --instance-id='<core-vke-instance-id-4>'
```

Review the deterministic JSON diff. It always shows the requested billable
node count and a cost notice. The lightweight default is one load-balancer
node; choose an odd higher count explicitly (for example, `--nodes=3`) when
the extra load-balancer-node availability justifies the additional cost.

After reviewing the plan, the same declaration can be applied by replacing
`--plan` with `--apply`:

```sh
go run ./cmd/vultr-dns-lb \
  --apply \
  --label='simpledns-authoritative-syd' \
  --instance-id='<core-vke-instance-id-1>' \
  --instance-id='<core-vke-instance-id-2>' \
  --instance-id='<core-vke-instance-id-3>' \
  --instance-id='<core-vke-instance-id-4>'
```

## Contract

- Region is fixed to `syd`; an exact-label match in another region fails
  closed.
- `--plan` is explicit but optional because read-only planning is the default.
- `--apply` is the only mutation mode; it is mutually exclusive with
  `--plan`.
- `--instance-id` is required and repeatable. IDs are normalized and duplicate
  values are rejected.
- `--nodes` defaults to `1` and must be odd from `1` through `99`.
- `--label` defaults to `simpledns-authoritative-syd`.
- `--wait-timeout` defaults to `5m`; `--poll-interval` defaults to `5s`.
- `VULTR_API_KEY` is the only environment variable read.

The managed configuration is TCP and UDP port `53` to the same protocols on
NodePort `30053`, a TCP health check on `30053`, the declared instance IDs and
node count, and public IPv4/IPv6 load-balancer firewall entries for port `53`.
Unrelated forwarding rules, firewall rules, and top-level load-balancer fields
are preserved. Duplicate exact-label matches abort without mutation. Apply
waits boundedly for `active` status and both assigned public addresses.

The payloads follow Vultr's official v2 documentation for
[provisioning](https://docs.vultr.com/products/network/load-balancer/provisioning),
[forwarding rules](https://docs.vultr.com/products/network/load-balancer/configuration/forwarding-rules),
and [load-balancer firewall rules](https://docs.vultr.com/products/network/load-balancer/configuration/networking/firewall).
