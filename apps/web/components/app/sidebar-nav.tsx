"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { KeyRound, LockKeyhole, RefreshCw } from "lucide-react";

import { BrandMark } from "@/components/app/brand-mark";
import { usePlatform } from "@/components/app/platform-provider";
import { useZones } from "@/components/app/zones-provider";
import { Button } from "@/components/ui/button";
import { StatusDot } from "@/components/ui/status-dot";
import { plural } from "@/lib/format";
import { lockOperatorSession } from "@/lib/operator-session";
import { cn } from "@/lib/utils";

export type SidebarNavProps = {
  /** Called from every link so the mobile Sheet can close without an effect. */
  onNavigate?: () => void;
};

const navItemClassName =
  "text-ui flex items-center justify-between gap-2 rounded-md px-2.5 py-2 text-subtle outline-none hover:bg-fill hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring aria-[current=page]:bg-fill-strong aria-[current=page]:font-medium aria-[current=page]:text-foreground";

type NavItem = { label: string; href: string };

const navItems: NavItem[] = [
  { label: "Domains", href: "/domains" },
  { label: "Mailboxes", href: "/mailboxes" },
  { label: "Deploys", href: "/deploys" },
  { label: "Activity", href: "/activity" },
];

const footerItems: NavItem[] = [
  { label: "Billing", href: "/billing" },
  { label: "Settings", href: "/settings" },
];

export function SidebarNav({ onNavigate }: SidebarNavProps) {
  const pathname = usePathname();
  const { refreshing, error, zones } = useZones();
  const { activeDeploys, invalidate } = usePlatform();
  // Counted from the store, not from the server status of the last list: a zone or
  // record written a moment ago is already here, while `server` lags until refresh.
  const zoneCount = zones.length;
  const recordCount = zones.reduce((count, zone) => count + zone.records.length, 0);

  const isActive = (href: string) => {
    if (href === "/domains") {
      // The connect flow is part of the Domains section.
      return (
        pathname === "/domains" ||
        pathname.startsWith("/domains/") ||
        pathname.startsWith("/connect")
      );
    }
    return pathname === href || pathname.startsWith(`${href}/`);
  };

  const renderItem = (item: NavItem) => (
    <Link
      key={item.href}
      href={item.href}
      onClick={onNavigate}
      aria-current={isActive(item.href) ? "page" : undefined}
      className={navItemClassName}
    >
      {item.label}
      {item.href === "/deploys" && activeDeploys > 0 ? (
        <StatusDot
          tone="info"
          pulse
          label={`${activeDeploys} ${plural(activeDeploys, "deploy")} running`}
        />
      ) : null}
    </Link>
  );

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-1.5">
      <Link
        href="/domains"
        onClick={onNavigate}
        className="flex items-center gap-2.5 rounded-sm px-2 pt-1.5 pb-[22px] outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        <BrandMark />
      </Link>

      <nav aria-label="Primary" className="flex flex-col gap-1.5">
        {navItems.map(renderItem)}
      </nav>

      <div className="flex-1" />

      <div className="flex flex-col gap-1.5">{footerItems.map(renderItem)}</div>

      <div className="mt-2 border-t border-sidebar-border pt-3.5">
        <div className="flex items-center gap-2.5 px-2.5">
          <div className="flex size-[26px] items-center justify-center rounded-full bg-[#2a2f3a]">
            <KeyRound className="size-3.5 text-subtle" aria-hidden="true" />
          </div>
          <span className="text-[13px] text-subtle">Operator</span>
          <span className="flex-1" />
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label="Refresh domains"
            disabled={refreshing}
            // `invalidate` refreshes the three engine lists AND the zone list, so one
            // click reloads everything on screen.
            onClick={() => {
              void invalidate();
            }}
          >
            <RefreshCw
              className={cn(refreshing && "motion-safe:animate-spin")}
            />
          </Button>
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label="Lock operator session"
            onClick={() => lockOperatorSession("manual")}
          >
            <LockKeyhole />
          </Button>
        </div>
        {/* Only the online/unavailable sentence is a live region. The counters change
            on every record write, and announcing them would interrupt the record
            dialog's own feedback with a line nobody asked to hear again; they stay
            visible text, and the dashboard reports them too. */}
        <p className="mt-2 flex flex-wrap items-center gap-1.5 px-2.5 text-xs text-muted-foreground">
          <StatusDot tone={error ? "destructive" : "success"} />
          <span role="status">
            {error ? "Control API unavailable" : "Control API online"}
          </span>
          <span>
            {` · ${zoneCount} ${plural(zoneCount, "zone")} · ${recordCount} ${plural(recordCount, "record")}`}
          </span>
        </p>
      </div>
    </div>
  );
}
