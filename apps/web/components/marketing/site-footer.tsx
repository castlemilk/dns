import Link from "next/link";

/**
 * Landing-page footer. The design's Docs and Privacy links have nothing behind them and
 * are dropped; "Status" is a plain anchor because a `next/link` to a route handler would
 * prefetch `/api/health` with RSC headers.
 */
export function SiteFooter() {
  return (
    <footer className="flex flex-wrap items-center justify-between gap-4 border-t border-line px-6 py-7 text-[13px] text-muted-foreground lg:px-16">
      <span>© simple</span>
      <div className="flex items-center gap-5">
        <Link
          href="/domains"
          className="rounded-sm outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
        >
          Console
        </Link>
        <a
          href="/api/health"
          className="rounded-sm outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
        >
          Status
        </a>
      </div>
    </footer>
  );
}
