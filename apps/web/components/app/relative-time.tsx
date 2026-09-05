"use client";

import { useSyncExternalStore, type ReactNode } from "react";

import { useNow } from "@/hooks/use-now";
import { formatDateTime, formatRelative } from "@/lib/format";

export type RelativeTimeProps = {
  date?: Date;
  className?: string;
  /** Rendered when there is no date. */
  fallback?: ReactNode;
};

const subscribeToNothing = () => () => {};
const clientSnapshot = () => true;
const serverSnapshot = () => false;

/**
 * Hydration-safe relative time: the server and the first client render both show the
 * absolute date, so the markup matches; the relative text appears after mounting and
 * then ticks once a minute.
 */
export function RelativeTime({
  date,
  className,
  fallback = "—",
}: RelativeTimeProps) {
  const mounted = useSyncExternalStore(
    subscribeToNothing,
    clientSnapshot,
    serverSnapshot,
  );
  const now = useNow(60_000);

  if (!date) {
    return <span className={className}>{fallback}</span>;
  }

  return (
    <time
      dateTime={date.toISOString()}
      title={formatDateTime(date)}
      className={className}
    >
      {mounted ? formatRelative(date, now) : formatDateTime(date)}
    </time>
  );
}
