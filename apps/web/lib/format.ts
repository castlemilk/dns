import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { timestampDate } from "@bufbuild/protobuf/wkt";

/**
 * Formatting helpers. Callers pass `now` (from useNow) so nothing reads the clock in a
 * render body — `Date.now()` there fails the react-hooks/purity lint rule.
 */

export const toDate = (ts?: Timestamp): Date | undefined =>
  ts ? timestampDate(ts) : undefined;

// Month names are spelled out rather than taken from Intl: CLDR renders September as
// "Sept" in en-GB on newer ICU builds, and the same text must come out of Node and
// every browser (the landing page ships a build-time label; RelativeTime renders a
// title attribute before it hydrates).
const monthNames = [
  "Jan",
  "Feb",
  "Mar",
  "Apr",
  "May",
  "Jun",
  "Jul",
  "Aug",
  "Sep",
  "Oct",
  "Nov",
  "Dec",
];

const pad = (n: number) => String(n).padStart(2, "0");

/** "02 Sep 26" */
export function formatShortDate(date: Date): string {
  return `${pad(date.getDate())} ${monthNames[date.getMonth()]} ${String(
    date.getFullYear(),
  ).slice(-2)}`;
}

/** "2 Sep 2026, 14:03" */
export function formatDateTime(date: Date): string {
  return `${date.getDate()} ${monthNames[date.getMonth()]} ${date.getFullYear()}, ${pad(
    date.getHours(),
  )}:${pad(date.getMinutes())}`;
}

export function formatRelative(date: Date, now = new Date()): string {
  const seconds = Math.floor((now.getTime() - date.getTime()) / 1000);
  if (seconds < 45) {
    return "just now";
  }
  if (seconds < 90) {
    return "1 minute ago";
  }
  if (seconds < 3600) {
    // 90–119 s still floors to 1; the singular is already covered above, so never
    // render "1 minutes ago".
    return `${Math.max(2, Math.floor(seconds / 60))} minutes ago`;
  }
  if (seconds < 7200) {
    return "1 hour ago";
  }
  if (seconds < 86400) {
    return `${Math.floor(seconds / 3600)} hours ago`;
  }
  if (seconds < 172800) {
    return "yesterday";
  }
  if (seconds < 2592000) {
    return `${Math.floor(seconds / 86400)} days ago`;
  }
  return formatShortDate(date);
}

export function formatUptime(since: Date, now = new Date()): string {
  const seconds = Math.max(0, Math.floor((now.getTime() - since.getTime()) / 1000));
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) {
    return `${minutes}m`;
  }
  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    const restMinutes = minutes % 60;
    return restMinutes ? `${hours}h ${restMinutes}m` : `${hours}h`;
  }
  const days = Math.floor(hours / 24);
  const restHours = hours % 24;
  if (days < 7 && restHours) {
    return `${days}d ${restHours}h`;
  }
  return `${days}d`;
}

export const plural = (n: number, noun: string, pluralForm = `${noun}s`) =>
  n === 1 ? noun : pluralForm;

export const formatBigInt = (n: bigint) => n.toLocaleString("en-US");

export const formatSecondsAgo = (ms: number) =>
  `${Math.max(0, Math.round(ms / 1000))}s ago`;
