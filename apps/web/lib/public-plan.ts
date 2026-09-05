/**
 * The landing page's only data source. `GET /public/v1/plan` is unauthenticated and
 * carries capability facts and the server-formatted price label — no hosts, ids or
 * secrets — so the marketing page can state what this deployment actually offers
 * instead of a build-time constant that would be a mocked number whenever billing is
 * not configured.
 *
 * Server-only by construction: `DNS_API_URL` has no `NEXT_PUBLIC_` prefix, so it is
 * empty in the browser bundle and this module must only ever be imported from a server
 * component (`app/page.tsx`). There is no `server-only` package in this workspace, so
 * the rule is a convention, not a compile error — do not call this from a
 * `"use client"` module.
 */
export type PublicPlan = {
  hostingConfigured: boolean;
  uploadsEnabled: boolean;
  mailConfigured: boolean;
  mailboxesPerDomain: number;
  billingConfigured: boolean;
  priceLabel: string;
  policyNote: string;
};

const requestTimeoutMs = 2_000;
const revalidateSeconds = 300;

function asBoolean(value: unknown): boolean {
  return value === true;
}

function asNumber(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) && value >= 0
    ? Math.floor(value)
    : 0;
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

/**
 * Never throws and never blocks a render for long: an unset `DNS_API_URL` makes no
 * request at all, and any failure (unreachable, timeout, non-2xx, unparsable body)
 * returns `undefined`, which the landing page renders as the DNS-only phase-1 copy.
 */
export async function fetchPublicPlan(): Promise<PublicPlan | undefined> {
  const base = process.env.DNS_API_URL?.trim().replace(/\/$/, "");
  if (!base) {
    return undefined;
  }

  try {
    const response = await fetch(`${base}/public/v1/plan`, {
      next: { revalidate: revalidateSeconds },
      signal: AbortSignal.timeout(requestTimeoutMs),
    });
    if (!response.ok) {
      return undefined;
    }
    const body: unknown = await response.json();
    if (typeof body !== "object" || body === null) {
      return undefined;
    }
    const plan = body as Record<string, unknown>;
    return {
      hostingConfigured: asBoolean(plan.hosting_configured),
      uploadsEnabled: asBoolean(plan.uploads_enabled),
      mailConfigured: asBoolean(plan.mail_configured),
      mailboxesPerDomain: asNumber(plan.mailboxes_per_domain),
      billingConfigured: asBoolean(plan.billing_configured),
      priceLabel: asString(plan.price_label),
      policyNote: asString(plan.policy_note),
    };
  } catch {
    return undefined;
  }
}
