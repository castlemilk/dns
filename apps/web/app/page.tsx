import type { Metadata } from "next";

import { LandingPage } from "@/components/marketing/landing-page";
import { fetchPublicPlan } from "@/lib/public-plan";

export const metadata: Metadata = {
  // Absolute so the "%s · Deep Hosting" template from the root layout is bypassed.
  title: { absolute: "deep hosting · your website, email and DNS" },
  description:
    "Web hosting, email and authoritative DNS for your domain — guided records, and the raw zone one click away.",
};

/**
 * How long the prerendered `/` may serve before it is regenerated. This has to live
 * here, on the route, and not only as `next: { revalidate }` on the plan fetch.
 *
 * At build time `DNS_API_URL` is unset, so `fetchPublicPlan` returns before it calls
 * `fetch` at all. A revalidate period carried solely by that fetch is therefore never
 * registered for this route, and `/` is prerendered as a permanently static page: the
 * DNS-only copy would be frozen into the image and no amount of runtime health would
 * ever replace it. Declaring the period on the segment makes regeneration a property
 * of the route rather than of a request that may not happen.
 *
 * Must stay a literal — Next.js statically analyses this value, so `60 * 5` is not
 * valid. Keep it equal to `revalidateSeconds` in `@/lib/public-plan`.
 */
export const revalidate = 300;

/**
 * Server component. `fetchPublicPlan` reads `GET /public/v1/plan`, so `/` is
 * prerendered at build time — where the control API is unreachable and the plan is
 * `undefined`, giving the DNS-only page — and tells the truth about this deployment
 * from the first regeneration onwards. Under-claiming in that window is the safe
 * direction: the page never advertises an engine before it has answered.
 */
export default async function Home() {
  const plan = await fetchPublicPlan();
  return <LandingPage plan={plan} />;
}
