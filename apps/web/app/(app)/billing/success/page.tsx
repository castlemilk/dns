import type { Metadata } from "next";

import { CheckoutSuccess } from "@/components/billing/checkout-success";

export const metadata: Metadata = { title: "Billing" };

// Stripe returns to `?session_id={CHECKOUT_SESSION_ID}`; `?return=` is the path the
// console should offer afterwards. Both are validated by the client component.
export default async function Page(props: PageProps<"/billing/success">) {
  const params = await props.searchParams;
  const sessionId = params["session_id"];
  const returnPath = params["return"];
  return (
    <CheckoutSuccess
      sessionId={typeof sessionId === "string" ? sessionId : undefined}
      returnPath={typeof returnPath === "string" ? returnPath : undefined}
    />
  );
}
