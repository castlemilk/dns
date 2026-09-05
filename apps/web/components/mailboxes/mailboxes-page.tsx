"use client";

import { useMemo, useState } from "react";

import { EngineNotConfigured } from "@/components/app/engine-not-configured";
import { EngineUnreachable } from "@/components/app/engine-unreachable";
import { PageHeader } from "@/components/app/page-header";
import { usePlatform } from "@/components/app/platform-provider";
import { useZones } from "@/components/app/zones-provider";
import { MailDomainSection } from "@/components/mailboxes/mail-domain-section";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import type { MailDomain } from "@/gen/mail/v1/mail_pb";
import { EngineKind } from "@/gen/platform/v1/platform_pb";
import { plural } from "@/lib/format";
import { formatBytes } from "@/lib/platform-model";

export type MailboxesPageProps = {
  /** `?domain=` from the server component; preselects the domain filter. */
  initialDomain?: string;
};

const allDomains = "__all__";

/**
 * Every domain bound to the mail server, with its mailboxes, forwarders and the records
 * the control plane keeps for it. The three states below are final (spec2 §8.1, §12.1)
 * — unreachable control API, loading, and no mail engine configured. Only the last one
 * says "isn't configured".
 */
export function MailboxesPage({ initialDomain }: MailboxesPageProps) {
  const { mail, engine, status, statusError, refreshStatus } = usePlatform();

  const domains = useMemo(
    () =>
      [...mail.items.values()].sort((a, b) =>
        a.zoneName.localeCompare(b.zoneName),
      ),
    [mail.items],
  );

  // A mailbox count comes from the mail engine, one list call per domain. When
  // a call fails that domain reports zero and says so with counts_available, so
  // the aggregate would state a total nobody knows: the domain count is the only
  // honest half of the line (spec2 §12.14). The per-domain flag catches the case
  // the engine row cannot — a reachable engine that failed one domain's read.
  const countsKnown =
    !mail.error &&
    engine(EngineKind.MAIL)?.reachable !== false &&
    domains.every((domain) => domain.countsAvailable);
  const subtitle = useMemo(
    () => (countsKnown ? summarise(domains) : domainsOnly(domains)),
    [countsKnown, domains],
  );
  const showCounts = mail.loaded && mail.configured;

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Mailboxes"
        subtitle={showCounts ? subtitle : undefined}
      />
      {statusError && !status ? (
        // The control API did not answer: nothing is known about the engine, so the
        // page says so instead of claiming it "isn't configured" (spec2 §8.1).
        <EngineUnreachable
          engine="mail"
          reason={statusError}
          onRetry={() => {
            void refreshStatus(true);
          }}
        />
      ) : !mail.loaded ? (
        <PageSkeleton />
      ) : !mail.configured ? (
        <EngineNotConfigured
          engine="mail"
          missingEnv={engine(EngineKind.MAIL)?.missingEnv ?? []}
        />
      ) : (
        <MailboxesBody initialDomain={initialDomain} domains={domains} />
      )}
    </div>
  );
}

function summarise(domains: MailDomain[]): string {
  const mailboxes = domains.reduce(
    (sum, domain) => sum + domain.mailboxCount,
    0,
  );
  const bytes = domains.reduce(
    (sum, domain) => sum + Number(domain.usedBytes),
    0,
  );
  return `${mailboxes} ${plural(mailboxes, "mailbox", "mailboxes")} across ${domains.length} ${plural(domains.length, "domain")} · ${formatBytes(bytes)}`;
}

/** The half of the summary that does not depend on the mail engine answering. */
function domainsOnly(domains: MailDomain[]): string {
  return `${domains.length} ${plural(domains.length, "domain")} · mailbox counts unavailable`;
}

function PageSkeleton() {
  return (
    <div
      data-slot="mailboxes-skeleton"
      role="status"
      aria-busy="true"
      className="flex flex-col gap-3"
    >
      <span className="sr-only">Loading mailboxes…</span>
      <Skeleton className="h-10 w-full" />
      <Skeleton className="h-10 w-full" />
      <Skeleton className="h-10 w-full" />
    </div>
  );
}

function MailboxesBody({
  initialDomain,
  domains,
}: {
  initialDomain?: string;
  domains: MailDomain[];
}) {
  const { mail, status } = usePlatform();
  const { zones } = useZones();
  const [domain, setDomain] = useState(initialDomain ?? "");

  const zoneByName = useMemo(
    () => new Map(zones.map((zone) => [zone.name, zone])),
    [zones],
  );
  const shown = domain
    ? domains.filter((entry) => entry.zoneName === domain)
    : domains;
  const mailboxLimit = status?.mailboxesPerDomain ?? 0;

  if (domains.length === 0) {
    return (
      <div className="text-ui rounded-[10px] border border-line px-5 py-8 text-muted-foreground">
        No mail domains yet — open a domain and choose Host mail here.
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-6">
      {mail.error ? (
        <p role="alert" className="text-xs text-destructive">
          {mail.error}
        </p>
      ) : null}

      {domains.length > 1 ? (
        <Select
          value={domain || allDomains}
          onValueChange={(value) => setDomain(value === allDomains ? "" : value)}
        >
          <SelectTrigger aria-label="Filter by domain" className="w-56">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={allDomains}>All domains</SelectItem>
            {domains.map((entry) => (
              <SelectItem key={entry.zoneId} value={entry.zoneName}>
                {entry.zoneName}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      ) : null}

      {shown.map((entry, index) => (
        <div key={entry.zoneId} className="flex flex-col gap-6">
          {index > 0 ? <Separator /> : null}
          <MailDomainSection
            domain={entry}
            zone={zoneByName.get(entry.zoneName)}
            mailboxLimit={mailboxLimit}
          />
        </div>
      ))}

      {mail.status?.deliveryStatsAvailable === false &&
      mail.status.deliveryStatsNote ? (
        <p className="text-xs text-muted-foreground">
          {mail.status.deliveryStatsNote}
        </p>
      ) : null}
    </div>
  );
}
