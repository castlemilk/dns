import type { Metadata } from "next";

import { LandingPage } from "@/components/marketing/landing-page";
import { fetchPublicPlan } from "@/lib/public-plan";

export const metadata: Metadata = {
  // Absolute so the "%s · Simple DNS" template from the root layout is bypassed.
  title: { absolute: "simple · your domain, website and email" },
  description:
    "Authoritative DNS with guided website and email records, and the raw zone one click away.",
};

/**
 * Server component. `fetchPublicPlan` reads `GET /public/v1/plan` with
 * `next: { revalidate: 300 }`, so `/` is prerendered at build time (where the control
 * API is unreachable and the plan is `undefined`, giving the DNS-only page) and
 * revalidated at runtime once the deployment's control plane answers.
 */
export default async function Home() {
  const plan = await fetchPublicPlan();
  return <LandingPage plan={plan} />;
}
