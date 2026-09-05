"use client";

import Link from "next/link";

import { RelativeTime } from "@/components/app/relative-time";
import {
  Card,
  CardContent,
  CardFooter,
  CardHeader,
  CardLink,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusDot } from "@/components/ui/status-dot";
import { Severity } from "@/gen/activity/v1/activity_pb";
import { useActivity } from "@/hooks/use-platform-resources";
import { toDate } from "@/lib/format";

export type RecentActivityProps = {
  zoneId: string;
  zoneName: string;
};

const shown = 8;

const severityTone = {
  [Severity.ERROR]: "destructive",
  [Severity.WARN]: "warning",
} as const;

/**
 * The last few events this control plane recorded for one domain. Every row is an
 * `activity.v1.Event` as the log stored it — the summary is written by the producer,
 * never reconstructed here.
 */
export function RecentActivity({ zoneId, zoneName }: RecentActivityProps) {
  // `useActivity` keys its request on the primitive values, so a fresh object here
  // does not restart the list.
  const { items, loaded, error } = useActivity({ zoneId });
  const events = items.slice(0, shown);

  return (
    <Card>
      <CardHeader>
        <CardTitle>Recent activity</CardTitle>
      </CardHeader>

      <CardContent>
        {!loaded ? (
          <div className="flex flex-col gap-2">
            <Skeleton className="h-5 w-full" />
            <Skeleton className="h-5 w-4/5" />
            <Skeleton className="h-5 w-3/5" />
          </div>
        ) : error ? (
          <p className="text-ui text-muted-foreground" role="alert">
            The activity log couldn&apos;t be read: {error}
          </p>
        ) : events.length === 0 ? (
          <p className="text-subtle leading-[1.55]">
            Nothing recorded for {zoneName} yet. Record changes, deploys,
            mailboxes and billing events appear here.
          </p>
        ) : (
          <ul className="flex flex-col gap-2.5">
            {events.map((event) => {
              const tone =
                severityTone[event.severity as keyof typeof severityTone];
              return (
                <li key={event.id} className="flex items-baseline gap-2.5">
                  {tone ? (
                    <StatusDot
                      tone={tone}
                      className="translate-y-[-1px]"
                      label={event.severity === Severity.ERROR ? "error" : "warning"}
                    />
                  ) : null}
                  <span className="min-w-0 flex-1 text-[13px] text-subtle">
                    {event.summary}
                  </span>
                  <RelativeTime
                    date={toDate(event.time)}
                    className="shrink-0 font-mono text-[12.5px] text-muted-foreground"
                  />
                </li>
              );
            })}
          </ul>
        )}
      </CardContent>

      <CardFooter>
        <CardLink asChild>
          <Link href={`/activity?domain=${encodeURIComponent(zoneName)}`}>
            All activity →
          </Link>
        </CardLink>
      </CardFooter>
    </Card>
  );
}
