"use client";

import { useState } from "react";

import { usePlatform } from "@/components/app/platform-provider";
import { Button } from "@/components/ui/button";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import { describeError } from "@/lib/errors";
import { plural } from "@/lib/format";
import { getBillingClient } from "@/lib/platform-client";
import { zoneHref } from "@/lib/dns-values";

export type BillingStepProps = {
  zone: Zone;
  onContinue(): void;
  onSkip(): void;
};

/**
 * The connect flow's Billing step. The price is `status.price.label` and nothing else —
 * this app has no build-time price — and the list of what a subscription covers names
 * only the engines this deployment actually runs.
 *
 * Subscribing is informational in this release, which the control plane's own policy
 * note says; skipping it leaves the domain fully working. "Not now" and the return from
 * checkout are the same destination (`onSkip` → the flow's `finish()`), so `onContinue`
 * is part of the frozen step contract without a second meaning here.
 */
export function BillingStep({ zone, onSkip }: BillingStepProps) {
  const { billing, hosting, mail, status } = usePlatform();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const mailboxes = status?.mailboxesPerDomain ?? 0;
  const included = [
    hosting.configured ? "hosting" : undefined,
    mail.configured && mailboxes > 0
      ? `${mailboxes} ${plural(mailboxes, "mailbox", "mailboxes")}`
      : undefined,
    "DNS",
  ].filter((part): part is string => part !== undefined);

  async function subscribe() {
    setBusy(true);
    setError(undefined);
    try {
      const response = await getBillingClient().createCheckoutSession({
        zoneId: zone.id,
        returnPath: zoneHref(zone.name),
      });
      let url: URL | undefined;
      try {
        url = new URL(response.url);
      } catch {
        url = undefined;
      }
      // https anywhere, or plain http when this deployment runs the fake provider,
      // whose checkout page is served by the control plane itself.
      const allowed =
        url !== undefined &&
        (url.protocol === "https:" ||
          (billing.status?.provider === "fake" && url.protocol === "http:"));
      if (!url || !allowed) {
        setError("Checkout returned an address this console won't open.");
        return;
      }
      window.location.assign(url.toString());
    } catch (caught) {
      setError(describeError(caught));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-2.5">
        <h1 className="text-[30px] leading-[1.15] font-semibold break-words">
          Subscribe {zone.name}
        </h1>
        <p className="text-[15px] leading-[1.6] text-subtle">
          {billing.status?.price?.label ?? "—"} — {included.join(", ")} included.{" "}
          {billing.status?.policyNote}
        </p>
      </div>

      {error ? (
        <p role="alert" className="text-ui text-destructive">
          {error}
        </p>
      ) : null}

      <div className="flex flex-wrap items-center justify-end gap-2.5">
        <Button
          type="button"
          variant="outline"
          size="lg"
          className="text-ui h-10 rounded-[7px] px-4"
          onClick={onSkip}
        >
          Not now
        </Button>
        <Button
          type="button"
          size="lg"
          className="text-ui h-10 rounded-[7px] px-4 font-semibold"
          disabled={busy}
          onClick={() => {
            void subscribe();
          }}
        >
          {busy ? "Opening checkout…" : "Subscribe with Stripe"}
        </Button>
      </div>
    </div>
  );
}
