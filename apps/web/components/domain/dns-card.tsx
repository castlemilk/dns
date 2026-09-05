import type { ReactNode } from "react";

import {
  Card,
  CardAction,
  CardContent,
  CardFooter,
  CardHeader,
  CardLink,
  CardTitle,
} from "@/components/ui/card";
import { StatusPill } from "@/components/ui/status-pill";
import { plural } from "@/lib/format";
import type { NsComparison } from "@/lib/nameserver-check";
import { cn } from "@/lib/utils";
import type { DnsGroup } from "@/lib/zone-model";

export type DnsCardProps = {
  zoneName: string;
  groups: DnsGroup[];
  total: number;
  rawShown: boolean;
  nameserverState?: NsComparison["state"];
  /**
   * A site is attached or mail is bound, so the hosting/mail reconcilers keep this
   * zone's website and email records up to date. Changes the lead paragraph only.
   */
  engineManaged?: boolean;
  /** The raw-zone toggle is rendered only when this is provided. */
  onToggleRaw?: () => void;
  onAddRecord?: () => void;
  onVerify?: () => void;
  onGroupSelect?: (group: DnsGroup) => void;
  /** Rendered inside CardFooter instead of the callback links. */
  footer?: ReactNode;
};

const groupRow =
  "text-ui flex items-center justify-between gap-3 rounded-lg bg-fill px-3 py-2.5";

/**
 * Pure summary of what the zone contains, grouped by purpose. The group labels are
 * readings of the records themselves — nothing claims to know who wrote them.
 */
export function DnsCard(props: DnsCardProps) {
  const {
    groups,
    total,
    rawShown,
    nameserverState,
    engineManaged = false,
    onToggleRaw,
    onAddRecord,
    onVerify,
    onGroupSelect,
    footer,
  } = props;

  const notPointed =
    nameserverState === "elsewhere" || nameserverState === "partial";
  const onlyManaged = groups.every((group) => group.key === "managed");
  const links = (
    <>
      {onAddRecord ? (
        <CardLink onClick={onAddRecord}>Add record</CardLink>
      ) : null}
      {onVerify ? <CardLink onClick={onVerify}>Verify a service</CardLink> : null}
      <span className="flex-1" />
      {onToggleRaw ? (
        <CardLink
          onClick={onToggleRaw}
          aria-expanded={rawShown}
          aria-controls="raw-zone"
        >
          {rawShown ? "Hide raw records" : "Show raw records"}
        </CardLink>
      ) : null}
    </>
  );
  const showFooter =
    footer !== undefined || onAddRecord || onVerify || onToggleRaw;

  return (
    <Card>
      <CardHeader>
        <CardTitle>DNS</CardTitle>
        <CardAction>
          {notPointed ? (
            <span className="text-xs text-warning">not pointed here yet</span>
          ) : null}
          <span className="text-xs whitespace-nowrap text-muted-foreground">
            {total} {plural(total, "record")}
          </span>
          <StatusPill tone={notPointed ? "warning" : "success"}>
            MANAGED
          </StatusPill>
        </CardAction>
      </CardHeader>

      <CardContent>
        <p className="text-subtle leading-[1.55]">
          {engineManaged
            ? "Records for your website and email are written and kept up to date automatically. Anything you add yourself is listed here."
            : "Records for your website and email are written by the guided setup. Anything you add yourself is listed here, and the raw zone is one click away."}
        </p>

        <div className="flex flex-col gap-1.5">
          {groups.map((group) =>
            onGroupSelect ? (
              <button
                key={group.key}
                type="button"
                title={group.title}
                onClick={() => onGroupSelect(group)}
                className={cn(
                  groupRow,
                  "w-full text-left transition-colors hover:bg-fill-strong focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
                )}
              >
                <span className="truncate">{group.label}</span>
                <span
                  className={cn(
                    "whitespace-nowrap",
                    group.detailTone === "success"
                      ? "text-success"
                      : "text-muted-foreground",
                  )}
                >
                  {group.detail}
                </span>
              </button>
            ) : (
              <div key={group.key} title={group.title} className={groupRow}>
                <span className="truncate">{group.label}</span>
                <span
                  className={cn(
                    "whitespace-nowrap",
                    group.detailTone === "success"
                      ? "text-success"
                      : "text-muted-foreground",
                  )}
                >
                  {group.detail}
                </span>
              </div>
            ),
          )}
        </div>

        {onlyManaged ? (
          <p className="text-xs text-muted-foreground">No other records yet.</p>
        ) : null}
      </CardContent>

      {showFooter ? <CardFooter>{footer ?? links}</CardFooter> : null}
    </Card>
  );
}
