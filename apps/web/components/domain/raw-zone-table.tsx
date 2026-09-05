import type { ReactNode } from "react";

import { CardLink } from "@/components/ui/card";
import { RecordType } from "@/gen/dns/v1/dns_pb";
import { recordTypeName } from "@/lib/dns-values";
import { cn } from "@/lib/utils";
import type { ClassifiedRecord } from "@/lib/zone-model";

export type RawZoneTableProps = {
  id?: string;
  zoneName: string;
  rows: ClassifiedRecord[];
  highlightIds?: string[];
  onExport?: () => void;
  onImport?: () => void;
  renderActions?: (row: ClassifiedRecord) => ReactNode;
};

/**
 * SOURCE says who wrote the record, not what it is for: only records the hosting or
 * mail engine owns are "automatic" (they are rewritten by a reconciler and refused to
 * hand edits). Everything else a user typed is "Custom", however website-shaped it is.
 */
function sourceCell(row: ClassifiedRecord): {
  label: string;
  className: string;
} {
  if (row.origin === "hosting") {
    return { label: "Website · automatic", className: "text-success" };
  }
  if (row.origin === "mail") {
    return { label: "Email · automatic", className: "text-success" };
  }
  if (row.source === "managed") {
    return { label: "Managed", className: "text-muted-foreground" };
  }
  return { label: "Custom", className: "text-link" };
}

/**
 * User records first, in the order the server returned them; the managed SOA and
 * apex NS records last, matching the DNS card's group order. Values are rendered
 * exactly as stored (quoted TXT, trailing dots) — this is the raw zone.
 */
function sortRows(rows: ClassifiedRecord[]): ClassifiedRecord[] {
  const user = rows.filter((row) => row.source !== "managed");
  const managed = rows
    .filter((row) => row.source === "managed")
    .sort((a, b) => {
      const rank = (row: ClassifiedRecord) =>
        row.record.type === RecordType.SOA ? 0 : 1;
      return rank(a) - rank(b) || a.record.value.localeCompare(b.record.value);
    });
  return [...user, ...managed];
}

export function RawZoneTable({
  id,
  zoneName,
  rows,
  highlightIds,
  onExport,
  onImport,
  renderActions,
}: RawZoneTableProps) {
  const sorted = sortRows(rows);
  const highlighted = new Set(highlightIds ?? []);

  return (
    // `scroll-mt-16` keeps `scrollIntoView({ block: "start" })` from parking the
    // header row under the sticky 56 px mobile top bar; that bar is `lg:hidden`,
    // so the margin is dropped again at `lg`.
    <section
      id={id}
      className="scroll-mt-16 overflow-hidden rounded-xl border border-line lg:scroll-mt-0"
    >
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-line-soft px-5 py-3.5">
        <h2 className="text-sm font-semibold">
          Raw zone · <span className="font-mono break-all">{zoneName}</span>
        </h2>
        {onExport || onImport ? (
          <div className="flex items-center gap-3.5 text-[13px]">
            {onExport ? (
              <CardLink onClick={onExport}>Export zone file</CardLink>
            ) : null}
            {onImport ? <CardLink onClick={onImport}>Import</CardLink> : null}
          </div>
        ) : null}
      </div>

      {/* `relative` matters: `sr-only` is position:absolute, so without a positioned
          ancestor inside this scroll container those labels resolve against the initial
          containing block, land at the far right of the 720 px table and give the whole
          page a horizontal scrollbar on narrow screens. */}
      <div className="relative overflow-x-auto">
        <table className="w-full min-w-[720px] table-fixed border-collapse">
          <thead>
            <tr className="border-b border-line-soft">
              <th className="eyebrow w-[90px] px-5 py-2 text-left">Type</th>
              <th className="eyebrow w-[22%] py-2 pr-3 text-left">Name</th>
              <th className="eyebrow py-2 pr-3 text-left">Value</th>
              <th className="eyebrow w-[90px] py-2 pr-3 text-left">TTL</th>
              <th className="eyebrow w-[150px] py-2 pr-3 text-left">Source</th>
              <th className="w-[52px] px-5 py-2">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {sorted.map((row) => {
              const source = sourceCell(row);
              return (
                <tr
                  key={row.record.id}
                  data-highlight={
                    highlighted.has(row.record.id) ? "true" : undefined
                  }
                  className="border-b border-line-faint font-mono text-[13px] transition-colors last:border-0 hover:bg-row-hover data-[highlight=true]:bg-primary/8"
                >
                  <td className="px-5 py-2.5 text-link">
                    {recordTypeName(row.record.type)}
                  </td>
                  <td className="py-2.5 pr-3">
                    <div className="truncate" title={row.record.name}>
                      {row.record.name}
                    </div>
                  </td>
                  <td className="py-2.5 pr-3">
                    <div
                      className="max-w-[420px] truncate text-soft"
                      title={row.record.value}
                    >
                      {row.record.value}
                    </div>
                  </td>
                  <td className="py-2.5 pr-3 text-muted-foreground tabular-nums">
                    {row.record.ttl}
                  </td>
                  <td
                    className={cn(
                      "text-label py-2.5 pr-3 font-sans",
                      source.className,
                    )}
                  >
                    {source.label}
                  </td>
                  <td className="px-5 py-2.5 text-right">
                    {renderActions?.(row)}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </section>
  );
}
