"use client";

import type { ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { StatusDot, type StatusDotTone } from "@/components/ui/status-dot";
import type { NameserverCheckState } from "@/hooks/use-nameserver-check";
import { cn } from "@/lib/utils";

export type NameserverStatusBannerProps = {
  check: NameserverCheckState;
  zoneName: string;
  /** Seconds since the last settled check, from the caller's clock. */
  secondsAgo?: number;
  onSkip: () => void;
  onCheckNow: () => void;
  className?: string;
};

type Tone = "info" | "warning" | "success";

const toneClassName: Record<Tone, string> = {
  info: "border border-primary/20 bg-primary/8",
  warning: "border border-warning/25 bg-warning/8",
  success: "border border-success/25 bg-success/8",
};

const dotTone: Record<Tone, StatusDotTone> = {
  info: "info",
  warning: "warning",
  success: "success",
};

function lookupReason(check: NameserverCheckState): string {
  if (check.failure) {
    return check.failure;
  }
  switch (check.result?.status) {
    case "timeout":
      return "the lookup timed out";
    case "servfail":
      return "the lookup failed at the resolver";
    default:
      return "the lookup failed";
  }
}

type Description = {
  tone: Tone;
  message: ReactNode;
  pulse: boolean;
  /**
   * What the live region says. It deliberately omits the "last check {n}s ago"
   * counter: that text changes every second, and a live region containing it would
   * re-announce the whole sentence on every tick.
   */
  announcement: string;
};

function describe(
  check: NameserverCheckState,
  zoneName: string,
  ago: string,
): Description {
  const { comparison } = check;

  // Nothing has settled yet: the first request is still in flight. A *re*-check keeps
  // the sentence it last settled on (the pulsing dot carries "checking" instead), so
  // the banner does not flip back to this first-run message every 30 seconds.
  if (check.lastCheckedAt === undefined) {
    const message = "Checking nameservers…";
    return { tone: "info", message, pulse: true, announcement: message };
  }

  if (comparison.state === "pointed") {
    const extras =
      comparison.extras.length > 0
        ? ` Your registrar still lists ${comparison.extras.join(", ")}; remove them when you can.`
        : "";
    const message = `Nameservers point here — ${zoneName} is served by this service.${extras}`;
    return {
      tone: "success",
      message,
      pulse: check.checking,
      announcement: message,
    };
  }

  if (comparison.state === "partial") {
    const counts = `${comparison.matched.length} of ${comparison.expected.length} nameservers point here.`;
    return {
      tone: "warning",
      message: `Checking every 30 seconds… last check ${ago}, ${counts}`,
      pulse: true,
      announcement: counts,
    };
  }

  if (comparison.state === "elsewhere") {
    const { providerLabel, observed } = comparison;
    const extra =
      observed.length > 1 && !providerLabel ? ` +${observed.length - 1}` : "";
    const provider = `${providerLabel ?? observed[0]}${extra}`;
    return {
      tone: "info",
      message: `Checking nameservers every 30 seconds… last check ${ago}, still ${provider}.`,
      pulse: true,
      announcement: `Nameservers still point to ${provider}.`,
    };
  }

  // Unknown: either the name publishes no nameservers, or the check itself failed.
  const status = check.result?.status;
  if (!check.failure && (status === "nxdomain" || status === "nodata")) {
    return {
      tone: "info",
      message: `Checking every 30 seconds… last check ${ago}, no nameservers are published for ${zoneName} yet.`,
      pulse: true,
      announcement: `No nameservers are published for ${zoneName} yet.`,
    };
  }

  // A permanent failure is not retried, so the banner must not promise another go.
  if (check.permanentFailure && check.failure) {
    const message = `Can't check ${zoneName} — ${check.failure}. Point its nameservers at this service and verify with \`dig NS ${zoneName}\` against your own resolver.`;
    return { tone: "warning", message, pulse: false, announcement: message };
  }

  const persistent =
    check.failures >= 3 ? ` If this persists, verify with \`dig NS ${zoneName}\`.` : "";
  const message = `Couldn't check just now (${lookupReason(check)}) — trying again in 30 seconds.${persistent}`;
  return { tone: "info", message, pulse: true, announcement: message };
}

/**
 * The live nameserver check. Every word here is derived from /api/nameservers and the
 * zone's own nameservers — the registrar is never named, because an NS lookup cannot
 * know it.
 */
export function NameserverStatusBanner({
  check,
  zoneName,
  secondsAgo,
  onSkip,
  onCheckNow,
  className,
}: NameserverStatusBannerProps) {
  const ago = secondsAgo === undefined ? "just now" : `${secondsAgo}s ago`;
  const { tone, message, pulse, announcement } = describe(check, zoneName, ago);

  return (
    <div
      data-slot="nameserver-status-banner"
      className={cn(
        "flex flex-col gap-3 rounded-[10px] px-[18px] py-4 sm:flex-row sm:items-center sm:gap-3.5",
        toneClassName[tone],
        className,
      )}
    >
      <StatusDot tone={dotTone[tone]} pulse={pulse} className="shrink-0" />
      <p className="text-ui min-w-0 flex-1 text-soft">{message}</p>
      {/* The announced sentence changes only when the check's outcome changes, so a
          screen reader hears each new result once instead of every second. */}
      <span role="status" className="sr-only">
        {announcement}
      </span>
      {/* The secondary actions stack in a bounded column so the live sentence keeps most
          of the banner's width instead of wrapping into a narrow strip. */}
      <div className="flex shrink-0 flex-col items-start gap-1 text-[13px] text-muted-foreground sm:max-w-[42%]">
        <span>
          Can&rsquo;t change nameservers right now?{" "}
          <button
            type="button"
            onClick={onSkip}
            className="link rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            Skip this step
          </button>
        </span>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={onCheckNow}
          disabled={check.checking}
        >
          {/* Visible label = accessible name (WCAG 2.5.3): the banner already says
              what is being checked, so no extra hidden words. */}
          Check now
        </Button>
      </div>
    </div>
  );
}
