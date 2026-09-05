import Link from "next/link";
import type { ReactNode } from "react";
import type { Timestamp } from "@bufbuild/protobuf/wkt";

import { RelativeTime } from "@/components/app/relative-time";
import { DeployPhase } from "@/gen/hosting/v1/hosting_pb";
import { formatBigInt, formatUptime, plural, toDate } from "@/lib/format";
import type { LatestDeploy, MailTotals } from "@/lib/dashboard-model";
import { buildDurationLabel, formatBytes } from "@/lib/platform-model";
import { defaultAttentionCta } from "@/lib/platform-model";
import type { Attention, Dashboard } from "@/lib/zone-model";

export type SummaryCardsProps = {
  attention: Attention[];
  latest?: Dashboard["latest"];
  server?: Dashboard["server"];
  /**
   * The most recent deploy across every site, when the hosting engine is configured and
   * has one. Present means card 2 is "Latest deploy" instead of "Latest change".
   */
  latestDeploy?: LatestDeploy;
  /**
   * Mailbox totals, when the mail engine is configured and any domain is bound. Present
   * means card 3 is "Mail" instead of "Server".
   */
  mailTotals?: MailTotals;
  /** Number of domains, for the "no warnings" line. */
  total: number;
  /**
   * Set when something an attention item could have come from did not report —
   * a configured engine whose list failed, or a platform status that never
   * arrived. An empty attention list is then absence of evidence, not evidence
   * of absence, so this sentence replaces "No warnings across N domains."
   */
  noWarningsNote?: string;
  /** Ticking clock from useNow(60_000), used for the server uptime. */
  now: Date;
};

function SummaryCard({
  label,
  value,
  children,
}: {
  label: string;
  value: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="flex flex-col gap-1.5 rounded-[10px] border border-line px-5 py-[18px]">
      <div className="text-label text-muted-foreground">{label}</div>
      <div className="text-[15px] font-medium">{value}</div>
      <div className="text-[13px] text-muted-foreground">{children}</div>
    </div>
  );
}

/**
 * The Mail card's headline. It never presents a partial sum as a total: when the mail
 * server answered for every bound domain it reads as a platform figure, when it answered
 * for some it says how many it covers, and when it answered for none there is no number
 * to show at all.
 */
function MailTotalsValue({ totals }: { totals: MailTotals }) {
  if (totals.counted === 0) {
    return (
      <span className="text-warning">
        Mailbox counts unavailable · {totals.domains}{" "}
        {plural(totals.domains, "domain")} bound
      </span>
    );
  }
  const across =
    totals.counted === totals.domains
      ? `${totals.domains} ${plural(totals.domains, "domain")}`
      : `${totals.counted} of ${totals.domains} ${plural(totals.domains, "domain")}`;
  return (
    <>
      {`${totals.mailboxes} ${plural(
        totals.mailboxes,
        "mailbox",
        "mailboxes",
      )} across ${across} · ${formatBytes(totals.usedBytes)}`}
    </>
  );
}

/**
 * The third line of the "Latest deploy" card. Every phase says what actually happened:
 * a terminal deploy that never went live never renders a live time, and an approximate
 * duration keeps its "≈" (the control plane observed it; the engine did not report it).
 */
function DeployLine({ latest }: { latest: LatestDeploy }) {
  const { deploy } = latest;
  const duration = buildDurationLabel(deploy);

  switch (deploy.phase) {
    case DeployPhase.QUEUED:
      return <>Queued</>;
    case DeployPhase.BUILDING:
    case DeployPhase.RELEASING:
      return <>Building…</>;
    case DeployPhase.FAILED:
      return (
        <>
          Failed <RelativeTime date={toDate(deploy.finishedAt as Timestamp | undefined)} />
          {deploy.reason ? ` · ${deploy.reason}` : ""}
        </>
      );
    case DeployPhase.ABANDONED:
      return <>Abandoned{deploy.reason ? ` · ${deploy.reason}` : ""}</>;
    case DeployPhase.LOST:
      return <>Lost — the hosting engine no longer reports this build.</>;
    default: {
      const liveAt = toDate(deploy.liveAt as Timestamp | undefined);
      return (
        <>
          <RelativeTime date={liveAt} />
          {duration ? ` · ${duration}` : ""}
        </>
      );
    }
  }
}

export function SummaryCards({
  attention,
  latest,
  server,
  latestDeploy,
  mailTotals,
  total,
  noWarningsNote,
  now,
}: SummaryCardsProps) {
  const first = attention[0];
  const others = attention.length - 1;

  return (
    <section aria-label="Summary" className="grid gap-4 md:grid-cols-3">
      <SummaryCard
        label="Needs attention"
        value={
          first
            ? first.zoneName
              ? `${first.zoneName} · ${first.message}`
              : first.message
            : noWarningsNote
              ? "Nothing in the DNS store needs attention"
              : "Nothing needs attention"
        }
      >
        {first ? (
          <>
            <Link href={first.href} className="link">
              {first.cta ?? defaultAttentionCta}
            </Link>
            {others > 0 ? <span> and {others} more</span> : null}
          </>
        ) : noWarningsNote ? (
          // Something that feeds this list never answered, so "no warnings" would
          // be a statement about data this console does not have.
          <span className="text-warning">{noWarningsNote}</span>
        ) : (
          // Absence of warnings is all that is known: nothing here claims a
          // domain has a website or email set up.
          `No warnings across ${total} ${plural(total, "domain")}.`
        )}
      </SummaryCard>

      {latestDeploy ? (
        <SummaryCard
          label="Latest deploy"
          value={
            <>
              {latestDeploy.zoneName}
              {latestDeploy.branch ? ` · ${latestDeploy.branch}` : ""}
              {latestDeploy.sha ? (
                <>
                  {" · @ "}
                  <span className="font-mono">{latestDeploy.sha}</span>
                </>
              ) : null}
            </>
          }
        >
          <DeployLine latest={latestDeploy} />
        </SummaryCard>
      ) : (
        <SummaryCard
          label="Latest change"
          value={
            latest ? (
              <>
                {latest.zoneName} · serial{" "}
                <span className="font-mono">{latest.serial}</span>
              </>
            ) : (
              "No changes yet"
            )
          }
        >
          {latest ? (
            <>
              <RelativeTime date={latest.updatedAt} /> · {latest.recordCount}{" "}
              {plural(latest.recordCount, "record")}
            </>
          ) : (
            "Create or import a zone to see activity here."
          )}
        </SummaryCard>
      )}

      {mailTotals ? (
        <SummaryCard label="Mail" value={<MailTotalsValue totals={mailTotals} />}>
          {/* A domain the mail server could not be read for is named here rather
              than folded into the figure above: it carries 0 on the wire and a
              sum would state a zero nobody measured. */}
          {mailTotals.counted < mailTotals.domains ? (
            <span className="text-warning">
              {`Counts unavailable for ${
                mailTotals.domains - mailTotals.counted
              } ${plural(
                mailTotals.domains - mailTotals.counted,
                "domain",
              )}${mailTotals.unknownReason ? ` — ${mailTotals.unknownReason}` : "."}`}
            </span>
          ) : (
            (mailTotals.note ?? (
              <Link href="/mailboxes" className="link">
                Open mailboxes →
              </Link>
            ))
          )}
        </SummaryCard>
      ) : (
        <SummaryCard
          label="Server"
          value={
            server
              ? // "received", not "answered": the counter increments for every packet
                // the authoritative server reads, including refused ones.
                `${formatBigInt(server.queries)} DNS ${plural(
                  Number(server.queries),
                  "query",
                  "queries",
                )} received`
              : "Status not available"
          }
        >
          {server
            ? `${server.zones} ${plural(server.zones, "zone")} · ${server.records} ${plural(
                server.records,
                "record",
              )}${server.startedAt ? ` · up ${formatUptime(server.startedAt, now)}` : ""}`
            : "The control API did not report status."}
        </SummaryCard>
      )}
    </section>
  );
}
