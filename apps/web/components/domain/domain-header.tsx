import Link from "next/link";
import type { ReactNode } from "react";

import { RelativeTime } from "@/components/app/relative-time";
import { StatusDot } from "@/components/ui/status-dot";
import type { DomainModel } from "@/lib/zone-model";

export type DomainHeaderProps = {
  zoneName: string;
  healthTone: DomainModel["tone"];
  healthLabel: string;
  serial: number;
  updatedAt?: Date;
  /** Static replacement for the ticking RelativeTime (used by the landing frame). */
  updatedLabel?: ReactNode;
  nameservers: string[];
  nameserverLine?: ReactNode;
  websiteUrl?: string;
  actions?: ReactNode;
  /**
   * Subscription line (§8.4) — filled by WP4. The landing frame passes a fixed string;
   * the domain view passes the live line. The phase-1 body ignores it.
   */
  billingLine?: ReactNode;
};

/**
 * Pure: every value is passed in, so the landing frame can render this on the server
 * with a fixed `updatedLabel` and no client hook ever runs under it.
 */
export function DomainHeader(props: DomainHeaderProps) {
  const {
    zoneName,
    healthTone,
    healthLabel,
    serial,
    updatedAt,
    updatedLabel,
    nameservers,
    nameserverLine,
    actions,
    billingLine,
  } = props;

  return (
    <div className="flex flex-col gap-4">
      <nav
        aria-label="Breadcrumb"
        className="flex flex-wrap gap-2 text-[13px] text-muted-foreground"
      >
        <Link href="/domains" className="hover:text-foreground">
          Domains
        </Link>
        <span aria-hidden="true">/</span>
        <span className="break-all text-foreground" aria-current="page">
          {zoneName}
        </span>
      </nav>

      <div className="flex flex-col gap-4 md:flex-row md:items-end md:justify-between">
        <div className="flex min-w-0 flex-col gap-2">
          <h1 className="text-[30px] leading-[1.1] font-semibold break-all">
            {zoneName}
          </h1>
          <div className="text-ui flex flex-wrap gap-x-4 gap-y-1 text-muted-foreground">
            <span className="inline-flex items-center">
              <StatusDot tone={healthTone} className="mr-1.5" />
              {healthLabel}
            </span>
            <span>
              Serial <span className="font-mono">{serial}</span> · updated{" "}
              {updatedLabel ?? <RelativeTime date={updatedAt} />}
            </span>
            {nameserverLine ?? (
              <span>
                Nameservers:{" "}
                <span className="font-mono break-all">
                  {nameservers.length ? nameservers.join(", ") : "—"}
                </span>
              </span>
            )}
            {/* Only rendered when billing is configured: a console without a billing
                engine says nothing about subscriptions. */}
            {billingLine ?? null}
          </div>
        </div>
        {actions ? (
          <div className="flex flex-wrap items-center gap-2.5">{actions}</div>
        ) : null}
      </div>
    </div>
  );
}
