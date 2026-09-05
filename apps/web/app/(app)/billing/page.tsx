import type { Metadata } from "next";

import { BillingPage } from "@/components/billing/billing-page";

export const metadata: Metadata = { title: "Billing" };

export default async function Page(props: PageProps<"/billing">) {
  const { canceled } = await props.searchParams;
  return (
    <BillingPage canceled={typeof canceled === "string" ? canceled : undefined} />
  );
}
