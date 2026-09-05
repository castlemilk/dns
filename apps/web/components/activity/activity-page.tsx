"use client";

import { useState } from "react";

import { PageHeader } from "@/components/app/page-header";
import { RelativeTime } from "@/components/app/relative-time";
import { useZones } from "@/components/app/zones-provider";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusDot } from "@/components/ui/status-dot";
import { Severity, type Event } from "@/gen/activity/v1/activity_pb";
import { useActivity } from "@/hooks/use-platform-resources";
import { toDate } from "@/lib/format";

export type ActivityPageProps = {
  /** `?domain=` from the server component; preselects the domain filter. */
  initialDomain?: string;
  /** `?kind=` from the server component; preselects the kind filter. */
  initialKind?: string;
};

/**
 * The append-only event log this control plane keeps. Every row is one
 * `activity.v1.Event`: the summary was written by whichever producer made the change,
 * the actor says who asked for it, and the details are the exact key/value pairs the
 * log stored. Nothing is reconstructed or inferred here.
 */
type KindGroup = { id: string; label: string; kinds: string[] };

// The kind prefixes come from the allowlist in spec2 §7.1; a filter never invents a
// kind the log cannot contain.
const kindGroups: KindGroup[] = [
  { id: "all", label: "All activity", kinds: [] },
  { id: "dns", label: "DNS", kinds: ["zone.", "dns."] },
  { id: "website", label: "Website", kinds: ["site.", "deploy.", "hosting."] },
  { id: "email", label: "Email", kinds: ["mail."] },
  { id: "billing", label: "Billing", kinds: ["billing."] },
  { id: "system", label: "System", kinds: ["engine.", "platform."] },
];

const allDomains = "__all__";

function groupFor(id?: string): KindGroup {
  return kindGroups.find((group) => group.id === id) ?? kindGroups[0];
}

export function ActivityPage({ initialDomain, initialKind }: ActivityPageProps) {
  const { zones, loadedAt: zonesLoadedAt } = useZones();
  const [domain, setDomain] = useState(initialDomain ?? "");
  // `?kind=` is validated against the groups above; anything else falls back to All.
  const [kindId, setKindId] = useState(() => groupFor(initialKind).id);

  const selectedZone = zones.find((zone) => zone.name === domain);
  const group = groupFor(kindId);
  const { items, loaded, error, nextCursor, refresh, loadMore } = useActivity({
    zoneId: selectedZone?.id,
    kinds: group.kinds,
  });

  // A `?domain=` for a zone this console cannot see would silently widen the filter to
  // every domain, so say so instead. Keyed on `loadedAt`, not `loaded`: the zone list
  // also settles on failure, and an empty list that never arrived says nothing about
  // whether this domain exists.
  const zonesUnknown = zonesLoadedAt === undefined;
  const unknownDomain =
    domain !== "" && !zonesUnknown && selectedZone === undefined;

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Activity"
        subtitle="Everything this control plane recorded — record changes, deploys, mailboxes and billing."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              void refresh();
            }}
          >
            Refresh
          </Button>
        }
      />

      <div className="flex flex-wrap items-center gap-2.5">
        <Select
          value={domain === "" ? allDomains : domain}
          onValueChange={(next) => setDomain(next === allDomains ? "" : next)}
        >
          <SelectTrigger className="w-[220px]" aria-label="Filter by domain">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={allDomains}>All domains</SelectItem>
            {zones.map((zone) => (
              <SelectItem key={zone.id} value={zone.name}>
                {zone.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        <Select value={kindId} onValueChange={setKindId}>
          <SelectTrigger className="w-[180px]" aria-label="Filter by kind">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {kindGroups.map((entry) => (
              <SelectItem key={entry.id} value={entry.id}>
                {entry.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {unknownDomain ? (
        <p className="text-ui text-warning" role="alert">
          There is no domain called {domain} on this control plane — showing
          every domain.
        </p>
      ) : domain !== "" && zonesUnknown ? (
        <p className="text-ui text-warning" role="alert">
          The domain list couldn&apos;t be read, so {domain} can&apos;t be
          matched to a zone — showing every domain.
        </p>
      ) : null}

      {!loaded ? (
        <div role="status" aria-busy="true" className="flex flex-col gap-2">
          <span className="sr-only">Loading activity…</span>
          <Skeleton className="h-12 w-full" />
          <Skeleton className="h-12 w-full" />
          <Skeleton className="h-12 w-full" />
          <Skeleton className="h-12 w-full" />
        </div>
      ) : error ? (
        <p className="text-ui text-warning" role="alert">
          The activity log couldn&apos;t be read: {error}
        </p>
      ) : items.length === 0 ? (
        <div className="text-ui rounded-[10px] border border-line px-5 py-8 text-muted-foreground">
          Nothing recorded yet. Changes to records, sites, mailboxes and billing
          appear here.
        </div>
      ) : (
        <>
          <ul className="overflow-hidden rounded-[10px] border border-line">
            {items.map((event) => (
              <EventRow key={event.id} event={event} />
            ))}
          </ul>
          {nextCursor ? (
            <div>
              <Button
                variant="outline"
                size="sm"
                onClick={() => {
                  void loadMore();
                }}
              >
                Older →
              </Button>
            </div>
          ) : null}
        </>
      )}
    </div>
  );
}

const severityTone = {
  [Severity.ERROR]: "destructive",
  [Severity.WARN]: "warning",
} as const;

function EventRow({ event }: { event: Event }) {
  const tone = severityTone[event.severity as keyof typeof severityTone];
  const details = Object.entries(event.details).sort(([a], [b]) =>
    a.localeCompare(b),
  );

  return (
    <li className="flex flex-col gap-1.5 border-b border-line-soft px-5 py-3.5 last:border-0">
      <div className="flex flex-wrap items-baseline gap-3">
        <RelativeTime
          date={toDate(event.time)}
          className="shrink-0 font-mono text-[12.5px] text-muted-foreground"
        />
        <span className="eyebrow shrink-0">{event.actor || "system"}</span>
        <span className="text-ui min-w-0 flex-1 break-words">
          {event.summary}
        </span>
        {event.zoneName ? (
          <span className="shrink-0 font-mono text-[12.5px] text-muted-foreground">
            {event.zoneName}
          </span>
        ) : null}
        {tone ? (
          <StatusDot
            tone={tone}
            label={event.severity === Severity.ERROR ? "error" : "warning"}
          />
        ) : null}
      </div>

      <details className="text-[12.5px] text-muted-foreground">
        <summary className="cursor-pointer rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring">
          <span className="font-mono">{event.kind}</span>
        </summary>
        <dl className="mt-1.5 grid grid-cols-[minmax(0,10rem)_1fr] gap-x-3 gap-y-1">
          {event.correlationId ? (
            <>
              <dt className="eyebrow">correlation</dt>
              <dd className="font-mono break-all text-soft">
                {event.correlationId}
              </dd>
            </>
          ) : null}
          {details.map(([key, value]) => (
            <div key={key} className="contents">
              <dt className="eyebrow break-all">{key}</dt>
              <dd className="font-mono break-all text-soft">{value}</dd>
            </div>
          ))}
          {!details.length && !event.correlationId ? (
            <>
              <dt className="eyebrow">details</dt>
              <dd className="text-muted-foreground">none recorded</dd>
            </>
          ) : null}
        </dl>
      </details>
    </li>
  );
}
