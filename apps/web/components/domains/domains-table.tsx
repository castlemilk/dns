import Link from "next/link";

import { RelativeTime } from "@/components/app/relative-time";
import { StatusDot } from "@/components/ui/status-dot";
import type { PlatformRow } from "@/lib/dashboard-model";
import type { Cell } from "@/lib/zone-model";
import { cn } from "@/lib/utils";

export type DomainsTableProps = {
  rows: PlatformRow[];
  /** The current search text; only used for the "no match" message. */
  query: string;
  /** The billing engine is configured, so the BILLING column means something. */
  billingColumn: boolean;
};

// "muted" is essential status text ("Not set up", "No mail", "www alias only"), so it
// uses text-muted-foreground (5.96:1 on the app surface). text-faint (#6b7280) measures
// 3.9:1 there — below WCAG AA for this size — and is reserved for decorative text.
const cellTone: Record<Cell["tone"], string> = {
  default: "text-subtle",
  warning: "text-warning",
  muted: "text-muted-foreground",
};

// Written out in full (never interpolated) so Tailwind sees every candidate.
const headerColumns = "grid-cols-[1.4fr_1fr_1fr_1fr_140px]";
const headerColumnsBilling = "grid-cols-[1.4fr_1fr_1fr_1fr_140px_140px]";
const rowColumns = "md:grid-cols-[1.4fr_1fr_1fr_1fr_140px]";
const rowColumnsBilling = "md:grid-cols-[1.4fr_1fr_1fr_1fr_140px_140px]";

/** One stacked cell below md, one grid cell from md up. */
function RowCell({
  label,
  cell,
}: {
  label: string;
  cell: Cell & { title?: string };
}) {
  return (
    <div role="cell" className="flex gap-2.5 md:block">
      <span className="eyebrow w-16 shrink-0 pt-0.5 md:hidden">{label}</span>
      <span className={cellTone[cell.tone]} title={cell.title}>
        {cell.text}
      </span>
    </div>
  );
}

export function DomainsTable({ rows, query, billingColumn }: DomainsTableProps) {
  if (rows.length === 0) {
    return (
      <div className="text-ui rounded-[10px] border border-line px-5 py-8 text-muted-foreground">
        {query.trim()
          ? `No domains match “${query.trim()}”.`
          : "No domains to show."}
      </div>
    );
  }

  return (
    <div
      role="table"
      aria-label="Domains"
      className="overflow-hidden rounded-[10px] border border-line"
    >
      <div role="rowgroup">
        <div
          role="row"
          className={cn(
            "eyebrow hidden border-b border-line bg-fill-faint px-5 py-2.5 md:grid",
            billingColumn ? headerColumnsBilling : headerColumns,
          )}
        >
          <div role="columnheader">Domain</div>
          <div role="columnheader">Website</div>
          <div role="columnheader">Email</div>
          <div role="columnheader">DNS</div>
          {billingColumn ? <div role="columnheader">Billing</div> : null}
          <div role="columnheader" className="text-right">
            Updated
          </div>
        </div>
      </div>

      <div role="rowgroup">
        {rows.map((row) => (
          <div
            key={row.name}
            role="row"
            className={cn(
              "text-ui relative grid grid-cols-1 gap-1.5 border-b border-line-soft px-5 py-4 last:border-0 hover:bg-row-hover md:items-center md:gap-0",
              billingColumn ? rowColumnsBilling : rowColumns,
            )}
          >
            <div role="cell" className="flex items-center gap-3">
              <StatusDot tone={row.tone} />
              {/* Stretched link: the whole row is clickable, with one tab stop. */}
              <Link
                href={row.href}
                // The ring is inset because the row spans the wrapper's full width and
                // the wrapper clips overflow — an outside ring would be cut off.
                className="text-sm font-medium text-foreground outline-none after:absolute after:inset-0 focus-visible:after:rounded-[10px] focus-visible:after:ring-2 focus-visible:after:ring-ring focus-visible:after:ring-inset"
              >
                {row.name}
              </Link>
            </div>

            <RowCell label="Website" cell={row.website} />
            <RowCell label="Email" cell={row.email} />
            <RowCell label="DNS" cell={row.dns} />
            {billingColumn && row.billing ? (
              <RowCell label="Billing" cell={row.billing} />
            ) : billingColumn ? (
              <div role="cell" aria-hidden="true" className="hidden md:block" />
            ) : null}

            <div role="cell" className="flex gap-2.5 md:block md:text-right">
              <span className="eyebrow w-16 shrink-0 pt-0.5 md:hidden">
                Updated
              </span>
              <RelativeTime
                date={row.updatedAt}
                className="font-mono text-[13px] text-muted-foreground"
              />
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
