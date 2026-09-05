import type { Timestamp } from "@bufbuild/protobuf/wkt";

import {
  SubscriptionState,
  type DomainBilling,
  type GetBillingStatusResponse,
} from "@/gen/billing/v1/billing_pb";
import {
  DeployPhase,
  SiteState,
  type Deploy,
  type GetHostingStatusResponse,
  type Site,
} from "@/gen/hosting/v1/hosting_pb";
import {
  MailDomainState,
  type GetMailStatusResponse,
  type MailDomain,
} from "@/gen/mail/v1/mail_pb";
import { formatShortDate, plural, toDate } from "@/lib/format";
import {
  derivePlatformAttention,
  isDeployActive,
  mergeAttention,
  resolvedCommit,
} from "@/lib/platform-model";
import type { Attention, Cell, Dashboard, DomainRow } from "@/lib/zone-model";

/**
 * The dashboard's engine layer: the phase-1 `Dashboard` (derived from the zone list)
 * joined with what the hosting, mail and billing facades reported.
 *
 * Pure by construction — the caller passes `now` (from `useNow`) — and honest by
 * construction: a cell only changes when the matching engine returned a row for that
 * zone, so an unconfigured or unreachable engine leaves the phase-1 text exactly as it
 * was rather than claiming a domain has no website or no mail.
 */

/** A billing cell carries a `title` for the "—" case, which needs a spoken meaning. */
export type BillingCell = Cell & { title?: string };

export type PlatformRow = DomainRow & { billing?: BillingCell };

export type LatestDeploy = {
  zoneName: string;
  /**
   * The branch or tag this deploy asked for, read off the deploy itself. Empty
   * when the request named a commit, and empty for an upload or a rollback,
   * which were made from no branch at all — the site's default branch is never
   * substituted here.
   */
  branch: string;
  /** Short commit: the engine's resolved one, or the requested revision when that is a commit. */
  sha?: string;
  deploy: Deploy;
};

export type MailTotals = {
  /** Mailboxes across the domains the mail server answered for; only these are summed. */
  mailboxes: number;
  /** Bound domains in total. */
  domains: number;
  /** Bound domains whose figures the mail server did answer for. */
  counted: number;
  usedBytes: bigint;
  /**
   * The reason the mail server gave for a domain it could not report, from
   * `MailDomain.reason`. Present only when `counted < domains`.
   */
  unknownReason?: string;
  /** `delivery_stats_note`, shown only when the engine cannot report delivery stats. */
  note?: string;
};

export type PlatformDashboard = {
  rows: PlatformRow[];
  attention: Attention[];
  latestDeploy?: LatestDeploy;
  mailTotals?: MailTotals;
  subtitle: string;
};

export type EnginesConfigured = {
  hosting: boolean;
  mail: boolean;
  billing: boolean;
};

export type EngineStatuses = {
  hosting?: GetHostingStatusResponse;
  mail?: GetMailStatusResponse;
  billing?: GetBillingStatusResponse;
};

/* -------------------------------------------------------------------------- */
/* Small helpers                                                               */
/* -------------------------------------------------------------------------- */

/**
 * Table-width relative text: `12m ago`, `3h ago`, `2d ago`, then the short date. The
 * long form (`12 minutes ago`) does not fit a 1fr dashboard cell, and the exact instant
 * is still available through the row's UPDATED column.
 */
export function compactRelative(date: Date, now: Date): string {
  const seconds = Math.floor((now.getTime() - date.getTime()) / 1000);
  if (seconds < 45) {
    return "just now";
  }
  if (seconds < 3600) {
    return `${Math.max(1, Math.floor(seconds / 60))}m ago`;
  }
  if (seconds < 86400) {
    return `${Math.floor(seconds / 3600)}h ago`;
  }
  if (seconds < 2592000) {
    return `${Math.floor(seconds / 86400)}d ago`;
  }
  return formatShortDate(date);
}

const shaPattern = /^[0-9a-f]{7,40}$/;

/** How much of a commit id the "Latest deploy" card shows. */
const shortShaLength = 7;

/** True only for a value that really is a commit id, never for `main` or `v1.2.0`. */
export function isCommitRevision(revision: string): boolean {
  return shaPattern.test(revision.trim());
}

const terminalFailure = (phase: DeployPhase) =>
  phase === DeployPhase.FAILED ||
  phase === DeployPhase.ABANDONED ||
  phase === DeployPhase.LOST;

/* -------------------------------------------------------------------------- */
/* Cells                                                                       */
/* -------------------------------------------------------------------------- */

function websiteCell(row: DomainRow, now: Date, site?: Site): Cell {
  if (!site) {
    return row.website;
  }
  if (site.zoneMissing) {
    return { text: "Zone missing", tone: "warning" };
  }
  if (site.state === SiteState.ATTACHING) {
    return { text: "Attaching…", tone: "default" };
  }
  if (site.state === SiteState.DETACHING) {
    return { text: "Detaching…", tone: "muted" };
  }

  const latest = site.latest;
  if (isDeployActive(latest)) {
    return {
      text: latest?.phase === DeployPhase.QUEUED ? "Queued" : "Building…",
      tone: "default",
    };
  }

  const live = site.live;
  if (live) {
    const at = toDate(live.liveAt as Timestamp | undefined);
    return {
      text: at ? `Live · ${compactRelative(at, now)}` : "Live",
      tone: "default",
    };
  }
  if (latest && terminalFailure(latest.phase)) {
    return { text: "Failed", tone: "warning" };
  }
  return { text: "Attached · no deploy", tone: "muted" };
}

function emailCell(row: DomainRow, mail?: MailDomain): Cell {
  if (!mail) {
    return row.email;
  }
  if (mail.zoneMissing) {
    return { text: "Zone missing", tone: "warning" };
  }
  switch (mail.state) {
    case MailDomainState.BINDING:
      return { text: "Binding…", tone: "default" };
    case MailDomainState.UNBINDING:
      return { text: "Unbinding…", tone: "muted" };
    default:
      break;
  }
  // "no mailboxes" is a claim about the mail server's contents; it may only be
  // made when the mail server answered. counts_available says whether it did.
  if (!mail.countsAvailable) {
    return { text: "Hosted · counts unavailable", tone: "warning" };
  }
  if (mail.mailboxCount > 0) {
    return {
      text: `${mail.mailboxCount} ${plural(mail.mailboxCount, "mailbox", "mailboxes")}`,
      tone: mail.state === MailDomainState.DEGRADED ? "warning" : "default",
    };
  }
  return { text: "Hosted · no mailboxes", tone: "muted" };
}

function billingCell(billing?: DomainBilling): BillingCell {
  const end = toDate(billing?.currentPeriodEnd as Timestamp | undefined);
  const cancelAt = toDate(billing?.cancelAt as Timestamp | undefined);

  switch (billing?.state) {
    case SubscriptionState.ACTIVE:
      return {
        text: end ? `Active · ${formatShortDate(end)}` : "Active",
        tone: "default",
      };
    case SubscriptionState.TRIALING:
      return {
        text: end ? `Trialing · ${formatShortDate(end)}` : "Trialing",
        tone: "default",
      };
    case SubscriptionState.CANCELING: {
      const at = cancelAt ?? end;
      return {
        text: at ? `Cancels ${formatShortDate(at)}` : "Cancels",
        tone: "warning",
      };
    }
    case SubscriptionState.PAST_DUE:
      return { text: "Past due", tone: "warning" };
    case SubscriptionState.INCOMPLETE:
      return { text: "Incomplete", tone: "warning" };
    case SubscriptionState.PENDING:
      return { text: "Checkout pending", tone: "default" };
    case SubscriptionState.CANCELED:
      return { text: "Canceled", tone: "muted" };
    default:
      return { text: "—", tone: "muted", title: "no subscription" };
  }
}

/**
 * One dashboard row with whatever the engines know about that zone. `billingConfigured`
 * has a default so the optional engine arguments can stay optional; it is passed
 * explicitly everywhere in the app.
 */
export function toPlatformRow(
  row: DomainRow,
  now: Date,
  site?: Site,
  mail?: MailDomain,
  billing?: DomainBilling,
  billingConfigured = false,
): PlatformRow {
  return {
    ...row,
    website: websiteCell(row, now, site),
    email: emailCell(row, mail),
    billing: billingConfigured ? billingCell(billing) : undefined,
  };
}

/* -------------------------------------------------------------------------- */
/* Cards                                                                       */
/* -------------------------------------------------------------------------- */

function deployInstant(deploy: Deploy): number {
  const at =
    toDate(deploy.liveAt as Timestamp | undefined) ??
    toDate(deploy.finishedAt as Timestamp | undefined) ??
    toDate(deploy.startedAt as Timestamp | undefined) ??
    toDate(deploy.requestedAt as Timestamp | undefined);
  return at ? at.getTime() : 0;
}

function pickLatestDeploy(sites: Site[]): LatestDeploy | undefined {
  let best: Deploy | undefined;
  let bestSite: Site | undefined;
  for (const site of sites) {
    for (const deploy of [site.latest, site.live]) {
      if (!deploy) {
        continue;
      }
      if (!best || deployInstant(deploy) > deployInstant(best)) {
        best = deploy;
        bestSite = site;
      }
    }
  }
  if (!best || !bestSite) {
    return undefined;
  }
  // Everything here is read off the deploy itself. `Site.branch` is the site's
  // *default* revision — what the next deploy would use — so falling back to it
  // would name a branch this build was not made from: an upload has no branch at
  // all, and a commit deploy may have come from any branch, or none.
  const revision = best.source?.revision ?? "";
  const commit = isCommitRevision(revision);
  const resolved = resolvedCommit(best);
  return {
    zoneName: best.zoneName || bestSite.zoneName,
    branch: commit ? "" : revision,
    // The engine's own resolved commit when it reported one, otherwise the
    // requested revision when that already is a commit.
    sha: resolved
      ? resolved.short
      : commit
        ? revision.slice(0, shortShaLength)
        : undefined,
    deploy: best,
  };
}

function mailTotalsOf(
  domains: MailDomain[],
  status?: GetMailStatusResponse,
): MailTotals | undefined {
  if (!domains.length) {
    return undefined;
  }
  // A domain whose engine read failed carries 0 and 0 on the wire — proto3
  // scalars have no absence — so counts_available is the only thing that
  // separates "no mailboxes" from "nobody could look". Summing it would turn a
  // failed read into a confident zero, so it is left out and counted instead.
  let mailboxes = 0;
  let usedBytes = BigInt(0);
  let counted = 0;
  let unknownReason: string | undefined;
  for (const domain of domains) {
    if (!domain.countsAvailable) {
      unknownReason ??= domain.reason || undefined;
      continue;
    }
    counted += 1;
    mailboxes += domain.mailboxCount;
    usedBytes += domain.usedBytes;
  }
  return {
    mailboxes,
    domains: domains.length,
    counted,
    usedBytes,
    unknownReason: counted < domains.length ? unknownReason : undefined,
    // Only shown when the engine says it cannot report delivery statistics; the note is
    // the engine's own sentence, never a substitute number.
    note:
      status && !status.deliveryStatsAvailable
        ? status.deliveryStatsNote || undefined
        : undefined,
  };
}

/**
 * Attention items that are not about one domain: the gateway change waiting for an
 * operator, and webhook events the billing queue could not apply.
 */
function globalAttention(
  configured: EnginesConfigured,
  status?: EngineStatuses,
): Attention[] {
  const items: Attention[] = [];
  const pending = status?.hosting?.gatewayPendingAddresses ?? [];
  if (configured.hosting && pending.length) {
    items.push({
      zoneName: "",
      severity: "warn",
      code: "gateway-pending",
      message: `the gateway hostname now resolves to ${pending.join(", ")}`,
      href: "/settings",
      cta: "Apply →",
    });
  }
  const dead = status?.billing?.deadWebhooks ?? 0;
  if (configured.billing && dead > 0) {
    items.push({
      zoneName: "",
      severity: "warn",
      code: "billing-webhooks-dead",
      message: `${dead} webhook ${plural(dead, "event")} could not be applied`,
      href: "/billing",
      cta: "Retry →",
    });
  }
  return items;
}

function indexByZoneName<T extends { zoneName: string }>(
  items: Map<string, T>,
): Map<string, T> {
  const byName = new Map<string, T>();
  for (const item of items.values()) {
    if (item.zoneName) {
      byName.set(item.zoneName, item);
    }
  }
  return byName;
}

/**
 * The dashboard the console renders. `sites`, `mail` and `billing` are the provider's
 * zone-id-keyed slices; they are re-indexed by zone name here because a `DomainRow`
 * only carries the name.
 */
export function derivePlatformDashboard(
  dashboard: Dashboard,
  sites: Map<string, Site>,
  mail: Map<string, MailDomain>,
  billing: Map<string, DomainBilling>,
  configured: EnginesConfigured,
  now: Date,
  status?: EngineStatuses,
): PlatformDashboard {
  const sitesByName = configured.hosting
    ? indexByZoneName(sites)
    : new Map<string, Site>();
  const mailByName = configured.mail
    ? indexByZoneName(mail)
    : new Map<string, MailDomain>();
  const billingByName = configured.billing
    ? indexByZoneName(billing)
    : new Map<string, DomainBilling>();

  const rows = dashboard.rows.map((row) =>
    toPlatformRow(
      row,
      now,
      sitesByName.get(row.name),
      mailByName.get(row.name),
      billingByName.get(row.name),
      configured.billing,
    ),
  );

  const platformAttention: Attention[] = [];
  for (const row of dashboard.rows) {
    platformAttention.push(
      ...derivePlatformAttention(
        row.name,
        sitesByName.get(row.name),
        mailByName.get(row.name),
        billingByName.get(row.name),
      ),
    );
  }
  platformAttention.push(...globalAttention(configured, status));

  const siteList = [...sitesByName.values()];
  const running = siteList.filter((site) => isDeployActive(site.latest)).length;

  return {
    rows,
    attention: mergeAttention(dashboard.attention, platformAttention),
    latestDeploy: configured.hosting ? pickLatestDeploy(siteList) : undefined,
    mailTotals: configured.mail
      ? mailTotalsOf([...mailByName.values()], status?.mail)
      : undefined,
    subtitle: running
      ? `${dashboard.subtitle} · ${running} ${plural(running, "deploy")} running`
      : dashboard.subtitle,
  };
}
