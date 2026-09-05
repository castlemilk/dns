import { lower, stripDot } from "@/lib/dns-values";
import { nameserverSuffixes } from "@/lib/service-presets";

/**
 * Wire types and pure comparison logic for the nameserver check served by
 * /api/nameservers. The polling hook lives in hooks/use-nameserver-check.ts.
 */

export type NsStatus =
  | "ok"
  | "nxdomain"
  | "nodata"
  | "timeout"
  | "servfail"
  | "error";

export type NameserverCheckResult = {
  domain: string;
  status: NsStatus;
  nameservers: string[];
  checkedAt: string;
  code?: string;
};

export type NsComparison = {
  state: "pointed" | "partial" | "elsewhere" | "unknown";
  expected: string[];
  matched: string[];
  missing: string[];
  extras: string[];
  observed: string[];
  providerLabel?: string;
};

/** Identifies the DNS host a set of nameservers belongs to — never the registrar. */
export function nameserverProviderLabel(hosts: string[]): string | undefined {
  for (const raw of hosts) {
    const host = stripDot(lower(raw));
    for (const [suffix, label] of nameserverSuffixes) {
      const matched = suffix.endsWith("-")
        ? host.includes(suffix)
        : host.endsWith(suffix);
      if (matched) {
        return label;
      }
    }
  }
  return undefined;
}

function unique(values: string[]): string[] {
  return [...new Set(values.map((value) => stripDot(lower(value))))].filter(
    (value) => value.length > 0,
  );
}

export function compareNameservers(
  expected: string[],
  result?: NameserverCheckResult,
): NsComparison {
  const exp = unique(expected);
  if (!result || result.status !== "ok" || result.nameservers.length === 0) {
    return {
      state: "unknown",
      expected: exp,
      matched: [],
      missing: exp,
      extras: [],
      observed: result?.nameservers ?? [],
    };
  }

  const observed = unique(result.nameservers);
  const matched = exp.filter((host) => observed.includes(host));
  const missing = exp.filter((host) => !observed.includes(host));
  const extras = observed.filter((host) => !exp.includes(host));

  return {
    state:
      missing.length === 0 ? "pointed" : matched.length ? "partial" : "elsewhere",
    expected: exp,
    matched,
    missing,
    extras,
    observed,
    providerLabel: nameserverProviderLabel(observed),
  };
}

/**
 * A non-2xx answer from /api/nameservers. `permanent` marks the answers that
 * repeating the request cannot change (the name is not one this route will ever
 * look up), so the caller stops polling instead of retrying for ever.
 */
export class NameserverCheckError extends Error {
  readonly status: number;
  readonly permanent: boolean;

  constructor(status: number, message: string, permanent: boolean) {
    super(message);
    this.name = "NameserverCheckError";
    this.status = status;
    this.permanent = permanent;
  }
}

export async function fetchNameserverCheck(
  domain: string,
  signal: AbortSignal,
): Promise<NameserverCheckResult> {
  const response = await fetch(
    `/api/nameservers?domain=${encodeURIComponent(domain)}`,
    { signal, cache: "no-store" },
  );
  if (!response.ok) {
    if (response.status === 400) {
      // The route refuses names outside the public DNS (RFC 6761 special-use
      // namespaces such as .test, .internal, .lan, .home.arpa). A zone can
      // legitimately be named that way here; the nameservers just cannot be
      // looked up from the internet, and asking again will never change that.
      throw new NameserverCheckError(
        400,
        "this name is not in the public DNS, so its nameservers can't be looked up",
        true,
      );
    }
    throw new NameserverCheckError(
      response.status,
      `Nameserver check failed (${response.status})`,
      false,
    );
  }
  return (await response.json()) as NameserverCheckResult;
}
