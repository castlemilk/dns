import { isIP } from "node:net";
import { Resolver } from "node:dns/promises";

import { isHostname, normaliseZoneName } from "@/lib/dns-values";
import type { NameserverCheckResult, NsStatus } from "@/lib/nameserver-check";

/**
 * GET /api/nameservers?domain=example.com
 *
 * Resolves the NS records published for a domain so the connect flow can tell
 * whether a registrar has been pointed here yet. Only NS lookups are made —
 * nothing is fetched — so there is no SSRF surface. Lookup failures are
 * reported as structured results, never as a 500.
 *
 * The route is unauthenticated, so it is deliberately narrow: only public
 * hostnames are resolved (special-use and internal suffixes are refused), the
 * lookup goes to fixed public resolvers rather than the pod's own resolver (so
 * it answers "what does the internet see?" and can never be used to probe
 * cluster-internal names), the answer never distinguishes "no such domain" from
 * "no NS records", and no resolver error code reaches the client.
 */

const jsonHeaders = {
  "Cache-Control": "no-store",
  "Content-Type": "application/json; charset=utf-8",
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: jsonHeaders });
}

/**
 * Special-use and internal namespaces (RFC 6761/6762/7686 plus the suffixes that
 * private networks and Kubernetes clusters use). Resolving these would turn the
 * route into an existence oracle for names the public DNS never answers for.
 */
const refusedTlds = new Set([
  "alt",
  "arpa",
  "corp",
  "home",
  "i2p",
  "internal",
  "intranet",
  "invalid",
  "lan",
  "local",
  "localdomain",
  "localhost",
  "onion",
  "private",
  "test",
]);

function isPublicHostname(domain: string): boolean {
  if (!isHostname(domain)) {
    return false;
  }
  const labels = domain.split(".");
  return !refusedTlds.has(labels[labels.length - 1]);
}

/**
 * Fixed public resolvers. `DNS_CHECK_RESOLVERS` (comma-separated IPs) lets a
 * deployment without egress to 1.1.1.1/8.8.8.8 point the check somewhere else.
 */
const defaultResolvers = ["1.1.1.1", "8.8.8.8"];

function configuredResolvers(): string[] {
  const configured = (process.env.DNS_CHECK_RESOLVERS ?? "")
    .split(",")
    .map((value) => value.trim())
    .filter((value) => isIP(value) !== 0);
  return configured.length > 0 ? configured : defaultResolvers;
}

/** Tiny in-memory limiter: 60 requests per minute per client. */
const requestsPerWindow = 60;
const windowMs = 60_000;
const pruneAfterMs = 5 * 60_000;
/** Hard cap so a flood of distinct keys cannot grow the map without bound. */
const maxBuckets = 10_000;
const buckets = new Map<string, { count: number; resetAt: number }>();
let prunedAt = 0;

function prune(now: number): void {
  prunedAt = now;
  for (const [client, bucket] of buckets) {
    if (now - bucket.resetAt > pruneAfterMs) {
      buckets.delete(client);
    }
  }
  // Still oversized: drop the least recently created entries (Map keeps insertion order).
  while (buckets.size > maxBuckets) {
    const oldest = buckets.keys().next();
    if (oldest.done) {
      break;
    }
    buckets.delete(oldest.value);
  }
}

function limited(key: string, now = Date.now()): boolean {
  // Amortised: at most one sweep per window instead of a full scan per request.
  if (now - prunedAt > windowMs || buckets.size > maxBuckets) {
    prune(now);
  }

  const bucket = buckets.get(key);
  if (!bucket || now >= bucket.resetAt) {
    buckets.set(key, { count: 1, resetAt: now + windowMs });
    return false;
  }

  bucket.count += 1;
  return bucket.count > requestsPerWindow;
}

/** Strips an optional port / brackets and keeps only real IP addresses. */
function toIp(value: string): string | undefined {
  let host = value.trim();
  const bracketed = /^\[(.+)\](?::[0-9]+)?$/.exec(host);
  if (bracketed) {
    host = bracketed[1];
  } else if (/^[0-9.]+:[0-9]+$/.test(host)) {
    host = host.slice(0, host.lastIndexOf(":"));
  }
  return isIP(host) !== 0 ? host.toLowerCase() : undefined;
}

/**
 * The limiter key must be a value the caller cannot choose. Every hop of
 * x-forwarded-for except the last one is client-supplied (a proxy appends the
 * address it saw, and Next fills the header from the socket only when it is
 * absent), so the last hop is the one to key on.
 */
function clientKey(request: Request): string {
  const forwarded = request.headers.get("x-forwarded-for");
  if (forwarded) {
    const hops = forwarded
      .split(",")
      .map((hop) => hop.trim())
      .filter(Boolean);
    const last = hops.length > 0 ? toIp(hops[hops.length - 1]) : undefined;
    if (last) {
      return last;
    }
  }
  return "local";
}

/**
 * Global cap on concurrent lookups: each one holds a c-ares resolver and sockets
 * for up to 6 s, so an unauthenticated burst must not be able to fan out.
 */
const maxInFlight = 16;
let inFlight = 0;

export async function GET(request: Request): Promise<Response> {
  const raw = new URL(request.url).searchParams.get("domain") ?? "";
  const domain = normaliseZoneName(raw);
  if (!isPublicHostname(domain)) {
    return json({ error: "invalid_domain" }, 400);
  }

  if (limited(clientKey(request))) {
    return json({ error: "rate_limited" }, 429);
  }

  if (inFlight >= maxInFlight) {
    return json({ error: "busy" }, 429);
  }
  inFlight += 1;

  const resolver = new Resolver({ timeout: 2500, tries: 2 });
  resolver.setServers(configuredResolvers());
  const checkedAt = new Date().toISOString();
  let handle: ReturnType<typeof setTimeout> | undefined;
  const timer = new Promise<never>((_, reject) => {
    handle = setTimeout(() => {
      resolver.cancel();
      reject(Object.assign(new Error("timeout"), { code: "ETIMEOUT" }));
    }, 6000);
  });

  try {
    const ns = await Promise.race([resolver.resolveNs(domain), timer]);
    const nameservers = [
      ...new Set(ns.map((name) => name.toLowerCase().replace(/\.$/, ""))),
    ].sort();
    return json({
      domain,
      status: "ok",
      nameservers,
      checkedAt,
    } satisfies NameserverCheckResult);
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code ?? "UNKNOWN";
    // "no such domain" and "no NS records" are reported the same way: both mean
    // "no nameservers are published yet" to the connect flow, and telling them
    // apart would let an anonymous caller test whether a name exists.
    const status: NsStatus =
      code === "ENOTFOUND" || code === "ENODATA"
        ? "nodata"
        : code === "ETIMEOUT" || code === "ECANCELLED"
          ? "timeout"
          : code === "ESERVFAIL" ||
              code === "EREFUSED" ||
              code === "ECONNREFUSED"
            ? "servfail"
            : "error";
    if (status !== "nodata") {
      // Resolver state stays server-side; the client only learns the category.
      console.warn(`nameserver check for ${domain} failed: ${code}`);
    }
    return json({
      domain,
      status,
      nameservers: [],
      checkedAt,
    } satisfies NameserverCheckResult);
  } finally {
    inFlight -= 1;
    // Without this every successful request leaves a 6 s timer behind that
    // would later cancel an idle resolver (and hold the event loop open).
    clearTimeout(handle);
  }
}
