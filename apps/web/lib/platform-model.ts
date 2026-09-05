import type { Timestamp } from "@bufbuild/protobuf/wkt";

import {
  SubscriptionState,
  type DomainBilling,
} from "@/gen/billing/v1/billing_pb";
import {
  DeployPhase,
  HostnameRole,
  HostnameState,
  SiteState,
  TimeSource,
  WwwMode,
  type Deploy,
  type EnvVar,
  type Hostname,
  type Site,
} from "@/gen/hosting/v1/hosting_pb";
import {
  MailDomainState,
  type MailDomain,
  type MailRecord,
} from "@/gen/mail/v1/mail_pb";
import { EngineKind } from "@/gen/platform/v1/platform_pb";
import { zoneHref } from "@/lib/dns-values";
import { formatDateTime, formatShortDate, plural, toDate } from "@/lib/format";
import type { Attention } from "@/lib/zone-model";

/**
 * Pure readings of the engine facades' messages. Nothing here invents a number: every
 * label is a rendering of a field the control plane returned, and a field the engine
 * did not report keeps its "not reported" wording rather than a plausible default.
 */

export type EngineName = "hosting" | "mail" | "billing";

/**
 * The tone vocabulary shared by StatusPill and StatusDot. Kept here as a plain union so
 * this module stays free of component imports.
 */
export type Tone = "success" | "warning" | "muted" | "info";

export const engineKindOf: Record<EngineName, EngineKind> = {
  hosting: EngineKind.HOSTING,
  mail: EngineKind.MAIL,
  billing: EngineKind.BILLING,
};

/* -------------------------------------------------------------------------- */
/* Sites and deploys                                                          */
/* -------------------------------------------------------------------------- */

export function siteStateLabel(state: SiteState): string {
  switch (state) {
    case SiteState.ATTACHING:
      return "Attaching";
    case SiteState.READY:
      return "Attached";
    case SiteState.DEGRADED:
      return "Degraded";
    case SiteState.DETACHING:
      return "Detaching";
    default:
      return "Unknown";
  }
}

export function siteStateTone(state: SiteState): Tone {
  switch (state) {
    case SiteState.READY:
      return "success";
    case SiteState.ATTACHING:
      return "info";
    case SiteState.DEGRADED:
      return "warning";
    default:
      return "muted";
  }
}

export function deployPhaseLabel(phase: DeployPhase): string {
  switch (phase) {
    case DeployPhase.QUEUED:
      return "Queued";
    case DeployPhase.BUILDING:
      return "Building";
    case DeployPhase.RELEASING:
      return "Releasing";
    case DeployPhase.LIVE:
      return "Live";
    case DeployPhase.SUPERSEDED:
      return "Superseded";
    case DeployPhase.FAILED:
      return "Failed";
    case DeployPhase.ABANDONED:
      return "Abandoned";
    case DeployPhase.LOST:
      return "Lost";
    default:
      return "Unknown";
  }
}

export function deployPhaseTone(phase: DeployPhase): Tone {
  switch (phase) {
    case DeployPhase.LIVE:
      return "success";
    case DeployPhase.QUEUED:
    case DeployPhase.BUILDING:
    case DeployPhase.RELEASING:
      return "info";
    case DeployPhase.FAILED:
    case DeployPhase.ABANDONED:
    case DeployPhase.LOST:
      return "warning";
    default:
      return "muted";
  }
}

/** QUEUED, BUILDING and RELEASING are the phases a poller has to keep watching. */
export function isDeployActive(deploy?: Deploy): boolean {
  return (
    deploy !== undefined &&
    (deploy.phase === DeployPhase.QUEUED ||
      deploy.phase === DeployPhase.BUILDING ||
      deploy.phase === DeployPhase.RELEASING)
  );
}

/** A site the control plane is still working on: poll it. */
export function isSiteActive(site?: Site): boolean {
  if (!site) {
    return false;
  }
  return (
    site.state === SiteState.ATTACHING ||
    site.state === SiteState.DETACHING ||
    isDeployActive(site.latest)
  );
}

export function apexHostname(site?: Site) {
  return site?.hostnames.find(
    (hostname) => hostname.role === HostnameRole.APEX,
  );
}

/**
 * Short reading of one hostname's state. The caller appends `reason` where the design
 * shows it; REGISTERED deliberately says the engine did not report readiness rather
 * than claiming a certificate exists.
 */
export function hostnameStateLabel(state: HostnameState): string {
  switch (state) {
    case HostnameState.CERTIFICATE_READY:
      return "On · auto-renewing";
    case HostnameState.ROUTES_READY:
      return "On · platform certificate";
    case HostnameState.CERTIFICATE_PENDING:
      return "Pending";
    case HostnameState.UNROUTED:
      return "Not routed — the hosting engine has no gateway configured";
    case HostnameState.REGISTERED:
      return "Requested automatically · status not reported by this hosting engine version";
    case HostnameState.MISSING:
      return "Hostname not registered";
    default:
      return "Not reported";
  }
}

export function hostnameStateTone(state: HostnameState): Tone {
  switch (state) {
    case HostnameState.CERTIFICATE_READY:
    case HostnameState.ROUTES_READY:
      return "success";
    case HostnameState.CERTIFICATE_PENDING:
    case HostnameState.UNROUTED:
    case HostnameState.MISSING:
      return "warning";
    default:
      return "muted";
  }
}

/**
 * True when the platform may link `https://<zone>`: the certificate is ready, the apex is
 * routed under the platform's shared certificate (`ROUTES_READY`, rendered on the domain
 * view as "On · platform certificate"), or this hosting engine version does not report
 * readiness at all (the old server, §2.3) — in which case the honest thing is to offer the
 * link rather than claim a failure.
 */
export function apexServesHttps(site?: Site): boolean {
  const apex = apexHostname(site);
  if (!apex) {
    return false;
  }
  return (
    apex.state === HostnameState.CERTIFICATE_READY ||
    apex.state === HostnameState.ROUTES_READY ||
    apex.state === HostnameState.REGISTERED
  );
}

/* -------------------------------------------------------------------------- */
/* Billing                                                                    */
/* -------------------------------------------------------------------------- */

export function subscriptionLabel(state: SubscriptionState): string {
  switch (state) {
    case SubscriptionState.ACTIVE:
      return "Active";
    case SubscriptionState.TRIALING:
      return "Trialing";
    case SubscriptionState.PAST_DUE:
      return "Past due";
    case SubscriptionState.CANCELING:
      return "Canceling";
    case SubscriptionState.CANCELED:
      return "Canceled";
    case SubscriptionState.PENDING:
      return "Checkout pending";
    case SubscriptionState.INCOMPLETE:
      return "Incomplete";
    case SubscriptionState.UNBILLED:
      return "No subscription";
    default:
      return "Unknown";
  }
}

export function subscriptionTone(state: SubscriptionState): Tone {
  switch (state) {
    case SubscriptionState.ACTIVE:
      return "success";
    case SubscriptionState.TRIALING:
    case SubscriptionState.PENDING:
      return "info";
    case SubscriptionState.PAST_DUE:
    case SubscriptionState.CANCELING:
    case SubscriptionState.INCOMPLETE:
      return "warning";
    default:
      return "muted";
  }
}

/* -------------------------------------------------------------------------- */
/* Numbers                                                                    */
/* -------------------------------------------------------------------------- */

const byteUnits = ["bytes", "KB", "MB", "GB", "TB", "PB"];

/**
 * Binary steps with the familiar labels: a 5 GiB Stalwart quota reads "5 GB", which is
 * how it was ordered. `bigint` is converted with `Number()` — mailbox sizes are far
 * below 2^53, and the generated TS types every 64-bit field as `bigint`.
 */
export function formatBytes(bytes: bigint | number): string {
  const value = Number(bytes);
  if (!Number.isFinite(value) || value <= 0) {
    return "0 bytes";
  }
  let scaled = value;
  let unit = 0;
  while (scaled >= 1024 && unit < byteUnits.length - 1) {
    scaled /= 1024;
    unit += 1;
  }
  if (unit === 0) {
    return `${Math.round(scaled)} bytes`;
  }
  return `${scaled >= 10 ? Math.round(scaled) : Math.round(scaled * 10) / 10} ${byteUnits[unit]}`;
}

/** Minor units → `"$6.00"`. Falls back to `"6.00 USD"` for an unknown currency code. */
export function formatMoney(minor: bigint | number, currency: string): string {
  const amount = Number(minor) / 100;
  const code = (currency || "usd").toUpperCase();
  try {
    return new Intl.NumberFormat("en-US", {
      style: "currency",
      currency: code,
    }).format(amount);
  } catch {
    return `${amount.toFixed(2)} ${code}`;
  }
}

/** `38s`, `4m 05s`, `1h 12m`. Used for build durations and the build deadline. */
export function formatDuration(seconds: number): string {
  const total = Math.max(0, Math.floor(seconds));
  if (total < 60) {
    return `${total}s`;
  }
  const minutes = Math.floor(total / 60);
  if (minutes < 60) {
    const rest = total % 60;
    return rest ? `${minutes}m ${String(rest).padStart(2, "0")}s` : `${minutes}m`;
  }
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  return rest ? `${hours}h ${rest}m` : `${hours}h`;
}

/**
 * `38s build` from the engine's own clock, `≈38s build` when the control plane only
 * observed the phases (§2.3). Returns undefined when no duration is known — never 0s.
 */
export function buildDurationLabel(deploy?: Deploy): string | undefined {
  if (!deploy || deploy.buildSeconds === 0) {
    return undefined;
  }
  const approx = deploy.timeSource !== TimeSource.ENGINE;
  return `${approx ? "≈" : ""}${formatDuration(deploy.buildSeconds)} build`;
}

export const observedTimesTitle =
  "Observed by the control plane; this hosting engine doesn't report build times.";

/* -------------------------------------------------------------------------- */
/* Not-configured copy                                                        */
/* -------------------------------------------------------------------------- */

const engineTitles: Record<EngineName, string> = {
  hosting: "Hosting isn't configured",
  mail: "Mail isn't configured",
  billing: "Billing isn't configured",
};

export function engineNotConfiguredCopy(
  kind: EngineName,
  missingEnv: string[],
): { title: string; body: string; names: string[] } {
  const names = missingEnv.filter((name) => name.trim().length > 0);
  const body = names.length
    ? `Set ${names.join(", ")} on the control API and restart it. Everything else keeps working without it.`
    : "Set its environment variables on the control API and restart it. Everything else keeps working without it.";
  return { title: engineTitles[kind], body, names };
}

/* -------------------------------------------------------------------------- */
/* Attention                                                                  */
/* -------------------------------------------------------------------------- */

export const defaultAttentionCta = "Finish setup →";

/**
 * Attention items that come from the engines, per zone. Every item is a statement about
 * a field the facade returned; nothing is derived from the absence of an engine.
 */
export function derivePlatformAttention(
  zoneName: string,
  site?: Site,
  mail?: MailDomain,
  billing?: DomainBilling,
): Attention[] {
  const items: Attention[] = [];
  const zone = zoneHref(zoneName);
  const add = (
    severity: Attention["severity"],
    code: string,
    message: string,
    href: string,
    cta?: string,
  ) => {
    items.push({ zoneName, severity, code, message, href, cta });
  };

  if (site) {
    if (site.zoneMissing) {
      add(
        "warn",
        "site-zone-missing",
        "a site is attached but this domain's zone no longer exists",
        zone,
        "Detach →",
      );
    } else if (site.state === SiteState.DEGRADED) {
      add(
        "warn",
        "site-degraded",
        site.reason || "the hosting engine reported a problem with this site",
        zone,
      );
    }
    const latest = site.latest;
    if (
      latest &&
      (latest.phase === DeployPhase.FAILED ||
        latest.phase === DeployPhase.ABANDONED)
    ) {
      add(
        "warn",
        "deploy-failed",
        latest.reason
          ? `last deploy failed — ${latest.reason}`
          : "the last deploy did not finish",
        `/deploys?domain=${encodeURIComponent(zoneName)}`,
        "Open deploy log →",
      );
    }
    if (site.dns && !site.dns.inSync) {
      add(
        "warn",
        "site-dns-drift",
        site.dns.problems[0] ?? "the website records have drifted",
        zone,
        "Re-apply records →",
      );
    }
    const apex = apexHostname(site);
    if (apex?.state === HostnameState.MISSING) {
      add(
        "warn",
        "hostname-missing",
        apex.retrying
          ? `${apex.host} isn't registered with the hosting engine — re-registering automatically`
          : `${apex.host} isn't registered with the hosting engine`,
        zone,
        apex.retrying ? undefined : "Retry →",
      );
    }
  }

  if (mail) {
    if (mail.zoneMissing) {
      add(
        "warn",
        "mail-zone-missing",
        "mail is bound but this domain's zone no longer exists",
        zone,
        "Unbind →",
      );
    } else if (mail.state === MailDomainState.DEGRADED) {
      add(
        "warn",
        "mail-degraded",
        mail.reason || "the mail server reported a problem with this domain",
        zone,
      );
    }
    if (!mail.zoneMissing && !mail.recordsInSync) {
      add(
        "warn",
        "mail-dns-drift",
        "the email records have drifted",
        zone,
        "Re-apply records →",
      );
    }
    if (mail.dkimPending) {
      add("info", "dkim-pending", "DKIM keys are still being generated", zone);
    }
    // has_postmaster is false both for "no postmaster" and for "the mail server
    // did not answer", so counts_available is what makes this an observation
    // rather than a guess.
    if (
      mail.state === MailDomainState.BOUND &&
      mail.countsAvailable &&
      !mail.hasPostmaster
    ) {
      add(
        "info",
        "postmaster-missing",
        `postmaster@${zoneName} doesn't exist yet`,
        zone,
      );
    }
  }

  if (billing) {
    switch (billing.state) {
      case SubscriptionState.PAST_DUE:
      case SubscriptionState.INCOMPLETE:
        add(
          "warn",
          "billing-past-due",
          "payment failed — update the card in the billing portal",
          "/billing",
          "Open billing →",
        );
        break;
      case SubscriptionState.CANCELING: {
        const at = toDate(billing.cancelAt as Timestamp | undefined);
        add(
          "info",
          "billing-canceling",
          at
            ? `subscription cancels ${formatShortDate(at)}`
            : "subscription is set to cancel",
          "/billing",
          "Open billing →",
        );
        break;
      }
      case SubscriptionState.UNBILLED:
        add(
          "info",
          "billing-unbilled",
          "no subscription yet",
          "/billing",
          "Subscribe →",
        );
        break;
      default:
        break;
    }
  }

  return items;
}

const severityRank = (item: Attention) => (item.severity === "warn" ? 0 : 1);
// The unbilled note is true of every domain nobody subscribed, so it never pushes a
// real problem down the list.
const isUnbilled = (item: Attention) => item.code === "billing-unbilled";

/**
 * Warnings first, then informational items, with the unbilled notes last and collapsed
 * into one row when more than one domain is unbilled.
 */
export function mergeAttention(
  zoneAttention: Attention[],
  platformAttention: Attention[],
): Attention[] {
  const all = [...zoneAttention, ...platformAttention];
  const unbilled = all.filter(isUnbilled);
  const rest = all.filter((item) => !isUnbilled(item));

  rest.sort((a, b) => {
    const bySeverity = severityRank(a) - severityRank(b);
    return bySeverity !== 0 ? bySeverity : a.zoneName.localeCompare(b.zoneName);
  });

  if (unbilled.length > 1) {
    const names = new Set(unbilled.map((item) => item.zoneName));
    return [
      ...rest,
      {
        zoneName: "",
        severity: "info",
        code: "billing-unbilled",
        message: `${names.size} ${plural(names.size, "domain")} ${
          names.size === 1 ? "has" : "have"
        } no subscription`,
        href: "/billing",
        cta: "Subscribe →",
      },
    ];
  }

  return [...rest, ...unbilled];
}

/* -------------------------------------------------------------------------- */
/* Mail: client-autoconfiguration records and delivery counts                 */
/* -------------------------------------------------------------------------- */

/**
 * The client-autoconfiguration records (the RFC 6186 / RFC 6764 `_tcp` SRV records and
 * the autoconfig/autodiscover aliases) are exactly the SRV and CNAME entries of the
 * mail engine's own record set: the policy records it publishes — MX, SPF, DKIM and
 * DMARC — are only ever MX and TXT.
 *
 * The copy reads the record set rather than `publish_client_autoconfig` on purpose.
 * The switch says what was asked for; these say what the engine actually put in the
 * plan, which is the only thing the "Not published" line is allowed to claim.
 */
export function isClientAutoconfigRecord(record: MailRecord): boolean {
  return record.type === "SRV" || record.type === "CNAME";
}

export function clientAutoconfigRecords(domain?: MailDomain): MailRecord[] {
  return (domain?.records ?? []).filter(isClientAutoconfigRecord);
}

/**
 * What the mail reconciler still does not write, named one by one, and why.
 *
 * MTA-STS would need an HTTPS policy endpoint under the customer's own name and TLSA
 * would need DANE keys, neither of which this deployment serves; CAA is a choice only
 * the domain's owner can make. SRV and the autoconfig/autodiscover aliases are listed
 * only while this binding genuinely has none of them — with the client-autoconfiguration
 * records on, they are published like everything else.
 */
export function mailNotPublished(domain?: MailDomain): {
  line: string;
  why: string;
} {
  const records = clientAutoconfigRecords(domain);
  const hasSrv = records.some((record) => record.type === "SRV");
  const hasAliases = records.some((record) => record.type === "CNAME");
  const missing = [
    ...(hasSrv ? [] : ["SRV"]),
    "MTA-STS",
    ...(hasAliases ? [] : ["autoconfig/autodiscover"]),
    "TLSA",
    "CAA",
  ];
  const last = missing[missing.length - 1];
  const line = `Not published: ${missing.slice(0, -1).join(", ")} and ${last}.`;
  const why =
    (hasSrv || hasAliases
      ? ""
      : "Mail client setup records are off for this domain. ") +
    "MTA-STS needs an HTTPS policy endpoint under your own domain and TLSA needs DANE keys, neither of which this deployment serves. CAA is yours to choose.";
  return { line, why };
}

/** "last hour" / "last 6 hours" — the window the counters actually covered. */
export function deliveryWindowLabel(hours: number): string {
  return hours === 1 ? "last hour" : `last ${hours} hours`;
}

export type DeliveryLine = {
  /** "118 delivered · 0 bounced" */
  text: string;
  /** What the pair counts, so a zero cannot read as "no mail at all". */
  label: string;
  /** The window these counts cover, from the engine — never a hard-coded 24. */
  window: string;
  title: string;
};

/**
 * The delivery line, or undefined whenever a number would be a guess: no delivery-event
 * receiver is configured, the engine marked the counts unavailable, or it reported a
 * window narrower than an hour. Undefined means the caller keeps the engine's own
 * `delivery_stats_note` instead of showing a zero nobody measured.
 *
 * The pair is mail this domain SENT. The control plane attributes each delivery event
 * to the envelope sender's bound zone and counts nothing inbound, because the commonest
 * inbound failure is refused at RCPT TO and never queued — a mixed pair would report a
 * bounce rate that does not exist. `delivery_stats_note` says the same on the wire.
 */
export function deliveryLine(domain?: MailDomain): DeliveryLine | undefined {
  const stats = domain?.delivery;
  if (!stats?.available || stats.windowHours < 1) {
    return undefined;
  }
  const since = toDate(stats.since);
  const counted = since
    ? `counted here since ${formatDateTime(since)}`
    : "counted here";
  return {
    text: `${stats.delivered} delivered · ${stats.bounced} bounced`,
    label: "Sent",
    window: deliveryWindowLabel(stats.windowHours),
    title: `Mail sent from this domain, from the mail server's own delivery events, ${counted}. Received mail is not counted.`,
  };
}

/* -------------------------------------------------------------------------- */
/* Hosting: resolved commit, www mode, environment variables                  */
/* -------------------------------------------------------------------------- */

/** How much of a commit id is shown; the full one always goes in a `title`. */
const shortCommitLength = 7;

export type ResolvedCommit = {
  /** The seven characters the design shows ("main @ 4f2c1a"). */
  short: string;
  /** The whole commit id, for the `title` attribute. */
  full: string;
};

/**
 * The commit the hosting engine actually built, or undefined when it reported
 * none.
 *
 * `Deploy.revision_resolved` is always populated: it carries the engine's own
 * commit when there is one and `source.revision` unchanged when there is not.
 * Equality with `source.revision` is therefore precisely "the engine reported
 * no commit" — an upload deploy, a build made before the engine could resolve
 * one, and an unreadable build manifest all land there. Nothing is rendered in
 * its place; the caller shows the requested revision alone, exactly as before.
 */
export function resolvedCommit(deploy?: Deploy): ResolvedCommit | undefined {
  const full = deploy?.revisionResolved.trim() ?? "";
  if (full === "" || full === (deploy?.source?.revision ?? "")) {
    return undefined;
  }
  return { short: full.slice(0, shortCommitLength), full };
}

export function wwwHostname(site?: Site): Hostname | undefined {
  return site?.hostnames.find(
    (hostname) => hostname.role === HostnameRole.WWW,
  );
}

export type WwwModeReading = {
  kind: "serve" | "redirect" | "pending" | "unreported";
  /** The www row's value. */
  text: string;
  /** Names what is missing or still outstanding; empty for a settled row. */
  note?: string;
};

/**
 * What `www.<zone>` does, read from `Hostname.redirect_to` — what the engine
 * reports — rather than from `Site.www_mode`, which is only what was asked for.
 *
 * Three fields are needed because two of the states are indistinguishable on
 * the wire: an engine that ignores `redirect_to` and an engine with nothing to
 * redirect both report an empty one. `capabilities.domain_redirect` turns true
 * only once the engine has actually reported a redirect somewhere, so a site
 * asking for a redirect with nothing reported and no such evidence is a
 * statement about the engine version, not about this host.
 */
export function wwwModeReading(
  zoneName: string,
  site: Site,
  domainRedirectSupported: boolean,
): WwwModeReading {
  const redirectTo = wwwHostname(site)?.redirectTo.trim() ?? "";
  if (redirectTo !== "") {
    return {
      kind: "redirect",
      text: redirectTo === zoneName ? "Redirects here" : `Redirects to ${redirectTo}`,
    };
  }
  if (site.wwwMode !== WwwMode.REDIRECT) {
    return { kind: "serve", text: "Serves the same site" };
  }
  if (!domainRedirectSupported) {
    return {
      kind: "unreported",
      text: "Redirect asked for · not reported",
      note: "This hosting engine version doesn't report host redirects, so what www does can't be shown here.",
    };
  }
  return {
    kind: "pending",
    text: "Serves the same site",
    note: "A redirect was asked for; the hosting engine doesn't report one on this host yet.",
  };
}

/** The environments a variable may belong to, in the order the console shows them. */
export const envEnvironments = [
  "production",
  "preview",
  "development",
] as const;

export type EnvEnvironment = (typeof envEnvironments)[number];

/** The default the facade applies to an empty environment. */
export const defaultEnvEnvironment: EnvEnvironment = "production";

/** "production" → "Production". An environment this console does not know is shown as it came. */
export function envEnvironmentLabel(environment: string): string {
  const name = environment.trim() || defaultEnvEnvironment;
  return name.charAt(0).toUpperCase() + name.slice(1);
}

export type EnvVarGroup = { environment: string; variables: EnvVar[] };

/**
 * The three environments in order, each with its variables sorted by name,
 * followed by any other environment the engine reported. Environments with no
 * variables are kept: the sheet says "none set" per environment rather than
 * hiding one the operator is about to add to.
 */
export function groupEnvVars(variables: EnvVar[]): EnvVarGroup[] {
  const byEnvironment = new Map<string, EnvVar[]>(
    envEnvironments.map((environment) => [environment, []]),
  );
  for (const variable of variables) {
    const environment = variable.environment.trim() || defaultEnvEnvironment;
    const existing = byEnvironment.get(environment);
    if (existing) {
      existing.push(variable);
    } else {
      byEnvironment.set(environment, [variable]);
    }
  }
  return [...byEnvironment.entries()].map(([environment, entries]) => ({
    environment,
    variables: [...entries].sort((a, b) => a.name.localeCompare(b.name)),
  }));
}
