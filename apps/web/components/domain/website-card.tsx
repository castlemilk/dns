import type { ReactNode } from "react";

import { CopyButton } from "@/components/app/copy-button";
import { RelativeTime } from "@/components/app/relative-time";
import {
  Card,
  CardContent,
  CardFooter,
  CardHeader,
  CardLink,
  CardTitle,
} from "@/components/ui/card";
import { StatusPill, type StatusPillTone } from "@/components/ui/status-pill";
import {
  DeployKind,
  DeployPhase,
  HostnameState,
  SiteState,
  type Deploy,
  type GetHostingStatusResponse,
  type Hostname,
  type Site,
} from "@/gen/hosting/v1/hosting_pb";
import { toDate } from "@/lib/format";
import type { NsComparison } from "@/lib/nameserver-check";
import {
  apexHostname,
  buildDurationLabel,
  observedTimesTitle,
  resolvedCommit,
  wwwModeReading,
} from "@/lib/platform-model";
import type { WebsiteStatus } from "@/lib/zone-model";

export type WebsiteCardProps = {
  zoneName: string;
  website: WebsiteStatus;
  /** Static replacement for the ticking RelativeTime (used by the landing frame). */
  updatedLabel?: ReactNode;
  onPoint?: () => void;
  onShowRecords?: () => void;
  /** Rendered inside CardFooter instead of the callback links. */
  footer?: ReactNode;

  /* Hosting engine (§8.4). Every line below is a reading of a field the hosting facade
     returned; a card rendered without them behaves exactly as it did in phase 1. */
  site?: Site;
  /** Only meaningful together with a `site` or `hostingStatus`. */
  hostingConfigured?: boolean;
  hostingStatus?: GetHostingStatusResponse;
  nameserverState?: NsComparison["state"];
  uploadsEnabled?: boolean;
  /** Static replacement for EVERY deploy-time RelativeTime in the card. */
  deployedLabel?: ReactNode;
  onAttach?: () => void;
  onDeploy?: () => void;
  onShowLog?: () => void;
  onRollback?: () => void;
  onDetach?: () => void;
  onReapplyDns?: () => void;
  onReregister?: () => void;
  onEnvironment?: () => void;
  onWwwMode?: () => void;
};

const pills: Record<
  WebsiteStatus["kind"],
  { tone: StatusPillTone; label: string }
> = {
  pointed: { tone: "success", label: "POINTED" },
  www_only: { tone: "warning", label: "WWW ONLY" },
  not_set_up: { tone: "muted", label: "NOT SET UP" },
};

const previewBoxClassName =
  "flex h-[110px] flex-col items-center justify-center gap-1 rounded-lg border border-line-soft bg-gradient-to-b from-[#1a1f28] to-[#141820] px-4 text-center";

function Row({ label, children }: { label: ReactNode; children: ReactNode }) {
  return (
    <div className="flex justify-between gap-4">
      <span className="text-muted-foreground">{label}</span>
      <span className="min-w-0 text-right break-words">{children}</span>
    </div>
  );
}

/* -------------------------------------------------------------------------- */
/* Readings of one deploy                                                      */
/* -------------------------------------------------------------------------- */

// Numbered groups only: tsconfig targets ES2017, where named groups are a compile
// error (TS1503).
const githubRepo =
  /^https:\/\/github\.com\/([A-Za-z0-9._-]+)\/([A-Za-z0-9._-]+?)(?:\.git)?$/;

function repositoryLabel(url: string): string | undefined {
  const match = githubRepo.exec(url.trim());
  return match ? `${match[1]}/${match[2]}` : undefined;
}

/** `github · owner/name` (linked) or `folder upload`; never a guess. */
function DeploySourceValue({ deploy }: { deploy?: Deploy }) {
  const source = deploy?.source;
  if (!source) {
    return <>—</>;
  }
  if (source.repository) {
    const label = repositoryLabel(source.repository);
    return (
      <span className="font-mono">
        github ·{" "}
        {label ? (
          <a
            href={source.repository}
            target="_blank"
            rel="noopener noreferrer"
            className="link"
          >
            {label}
          </a>
        ) : (
          <span className="break-all">{source.repository}</span>
        )}
      </span>
    );
  }
  if (deploy?.kind === DeployKind.UPLOAD || source.uploadName) {
    return <span className="font-mono">folder upload</span>;
  }
  return <>—</>;
}

/* -------------------------------------------------------------------------- */
/* The records-only card (phase 1, plus the engine's honest notes)             */
/* -------------------------------------------------------------------------- */

function RecordsCard({
  zoneName,
  website,
  updatedLabel,
  onPoint,
  onShowRecords,
  footer,
  note,
  setupParagraph,
  extraLinks,
}: {
  zoneName: string;
  website: WebsiteStatus;
  updatedLabel?: ReactNode;
  onPoint?: () => void;
  onShowRecords?: () => void;
  footer?: ReactNode;
  /** A muted sentence about the hosting engine, appended under the rows. */
  note?: ReactNode;
  /** Replaces the phase-1 "nothing points here yet" paragraph. */
  setupParagraph?: ReactNode;
  /** Engine links rendered before the phase-1 ones. */
  extraLinks?: ReactNode;
}) {
  const pill = pills[website.kind];
  const shown = website.apex.length > 0 ? website.apex : website.www.targets;
  const caption =
    website.kind === "pointed"
      ? `${zoneName} points to`
      : website.kind === "www_only"
        ? `www.${zoneName} points to`
        : "no address yet";
  const hasRecords =
    website.apex.length > 0 ||
    website.www.targets.length > 0 ||
    website.caa.length > 0;
  const wwwValues = website.www.targets.map((target) => target.value).join(", ");
  const links = (
    <>
      {extraLinks}
      {onPoint ? (
        <CardLink onClick={onPoint}>
          {website.kind === "not_set_up"
            ? "Point website here"
            : "Change where it points"}
        </CardLink>
      ) : null}
      {onShowRecords && hasRecords ? (
        <CardLink onClick={onShowRecords}>Show records</CardLink>
      ) : null}
    </>
  );
  const showFooter =
    footer !== undefined ||
    extraLinks !== undefined ||
    onPoint ||
    (onShowRecords && hasRecords);

  return (
    <Card>
      <CardHeader>
        <CardTitle>Website</CardTitle>
        <StatusPill tone={pill.tone}>{pill.label}</StatusPill>
      </CardHeader>

      <CardContent>
        <div className={previewBoxClassName}>
          <span className="text-xs text-muted-foreground">{caption}</span>
          {website.kind !== "not_set_up" && shown.length ? (
            <span className="max-w-full truncate font-mono text-[15px] text-foreground">
              {shown[0].value}
              {shown.length > 1 ? ` +${shown.length - 1}` : ""}
            </span>
          ) : null}
        </div>

        {website.kind === "not_set_up" ? (
          setupParagraph ?? (
            <p className="text-subtle leading-[1.55]">
              {website.www.kind === "dangling"
                ? `www.${zoneName} is an alias of ${zoneName}, which has no address yet. `
                : ""}
              Nothing points here yet. Enter the address your host gave you and
              the A, AAAA and www records are written for you.
            </p>
          )
        ) : (
          // 14 px, matching the design's card body, which spaces the preview box
          // and every row alike.
          <div className="flex flex-col gap-3.5">
            <Row label={zoneName}>
              {website.apex.length ? (
                <span className="font-mono">
                  {website.apex.map((target) => target.value).join(", ")}
                </span>
              ) : (
                <span className="text-warning">Not set</span>
              )}
            </Row>
            <Row label={`www.${zoneName}`}>
              {website.www.kind === "same" ? (
                `Same as ${zoneName}`
              ) : website.www.kind === "elsewhere" ? (
                <>
                  <span className="font-mono">→ {wwwValues}</span>
                  {website.www.service ? ` (${website.www.service})` : ""}
                </>
              ) : website.www.kind === "dangling" ? (
                <span className="text-warning">
                  Alias of {zoneName} (no address)
                </span>
              ) : (
                <span
                  className={
                    website.kind === "pointed"
                      ? "text-warning"
                      : "text-muted-foreground"
                  }
                >
                  Not set
                </span>
              )}
            </Row>
            <Row label="Records">
              {website.types.length || website.caa.length
                ? [
                    ...website.types,
                    ...(website.caa.length ? ["CAA"] : []),
                  ].join(", ")
                : "—"}
            </Row>
            {website.caa.length ? (
              <Row label="Certificates">
                <span className="font-mono">
                  {website.caa
                    .map((rule) => `${rule.tag} ${rule.value}`)
                    .join(", ")}
                </span>
              </Row>
            ) : null}
            <Row label="Last changed">
              {updatedLabel ?? <RelativeTime date={website.updatedAt} />}
            </Row>
            {website.wildcard ? (
              <Row label="Wildcard">
                <span className="font-mono">
                  *.{zoneName} →{" "}
                  {website.wildcard.map((target) => target.value).join(", ")}
                </span>
              </Row>
            ) : null}
          </div>
        )}

        {note ? (
          <p className="text-xs text-muted-foreground leading-[1.55]">{note}</p>
        ) : null}
      </CardContent>

      {showFooter ? <CardFooter>{footer ?? links}</CardFooter> : null}
    </Card>
  );
}

/* -------------------------------------------------------------------------- */
/* The attached-site card                                                      */
/* -------------------------------------------------------------------------- */

type SitePhase =
  | "attaching"
  | "detaching"
  | "zone-missing"
  | "no-deploy"
  | "queued"
  | "building"
  | "live"
  | "live-after-failure"
  | "failed";

function sitePhaseOf(site: Site): SitePhase {
  if (site.state === SiteState.DETACHING) {
    return "detaching";
  }
  if (site.state === SiteState.ATTACHING) {
    return "attaching";
  }
  if (site.zoneMissing) {
    return "zone-missing";
  }
  const latest = site.latest;
  if (!latest) {
    return site.live ? "live" : "no-deploy";
  }
  switch (latest.phase) {
    case DeployPhase.QUEUED:
      return "queued";
    case DeployPhase.BUILDING:
    case DeployPhase.RELEASING:
      return "building";
    case DeployPhase.FAILED:
    case DeployPhase.ABANDONED:
    case DeployPhase.LOST:
      return site.live ? "live-after-failure" : "failed";
    default:
      return site.live || latest.phase === DeployPhase.LIVE
        ? "live"
        : "no-deploy";
  }
}

function livePill(apex?: Hostname): { tone: StatusPillTone; label: string } {
  switch (apex?.state) {
    case HostnameState.UNROUTED:
      return { tone: "warning", label: "LIVE · NOT ROUTED" };
    case HostnameState.MISSING:
      return { tone: "warning", label: "LIVE · NOT REGISTERED" };
    case HostnameState.CERTIFICATE_PENDING:
      return { tone: "warning", label: "LIVE · CERT PENDING" };
    default:
      return { tone: "success", label: "LIVE" };
  }
}

function SiteCard(props: WebsiteCardProps) {
  const {
    zoneName,
    website,
    site,
    hostingStatus,
    nameserverState,
    deployedLabel,
    footer,
    onDeploy,
    onShowLog,
    onRollback,
    onDetach,
    onReapplyDns,
    onReregister,
    onEnvironment,
    onWwwMode,
  } = props;
  if (!site) {
    return null;
  }

  const phase = sitePhaseOf(site);
  const apex = apexHostname(site);
  const serving = site.live;
  const latest = site.latest;
  const shownDeploy = serving ?? latest;
  const servingCommit = resolvedCommit(serving);
  const deployTime = (date?: Date) =>
    deployedLabel ?? <RelativeTime date={date} />;

  const pill: { tone: StatusPillTone; label: string; pulse?: boolean } =
    phase === "attaching"
      ? { tone: "info", label: "ATTACHING", pulse: true }
      : phase === "detaching"
        ? { tone: "muted", label: "DETACHING", pulse: true }
        : phase === "zone-missing"
          ? { tone: "warning", label: "DEGRADED" }
          : phase === "no-deploy"
            ? { tone: "info", label: "ATTACHED" }
            : phase === "queued"
              ? { tone: "info", label: "QUEUED", pulse: true }
              : phase === "building"
                ? { tone: "info", label: "BUILDING", pulse: true }
                : phase === "failed"
                  ? {
                      tone: "warning",
                      label:
                        latest?.phase === DeployPhase.LOST
                          ? "UNKNOWN"
                          : "FAILED",
                    }
                  : livePill(apex);

  /* ---------------------------------------------------------------- rows -- */

  const branchRow = (() => {
    // The commit has to be read off the same deploy the revision came from, or
    // the row would pair one build's branch with another's commit.
    const deploy = serving?.source?.revision
      ? serving
      : latest?.source?.revision
        ? latest
        : undefined;
    const revision = deploy?.source?.revision ?? "";
    if (revision) {
      const commit = resolvedCommit(deploy);
      return (
        <Row label="Branch">
          {revision}
          {commit ? (
            <>
              {" @ "}
              <span className="font-mono" title={commit.full}>
                {commit.short}
              </span>
            </>
          ) : null}
        </Row>
      );
    }
    return (
      <Row label="Default branch">
        {site.branch ? <span className="font-mono">{site.branch}</span> : "—"}
      </Row>
    );
  })();

  const lastDeployRow = (() => {
    if (!latest && !serving) {
      return <Row label="Last deploy">No deploys yet</Row>;
    }
    if (phase === "queued" && latest) {
      return (
        <Row label="Last deploy">
          Queued · requested {deployTime(toDate(latest.requestedAt))}
        </Row>
      );
    }
    if (phase === "building" && latest) {
      return (
        <Row label="Last deploy">
          Building… started{" "}
          {deployTime(toDate(latest.startedAt ?? latest.requestedAt))}
        </Row>
      );
    }
    if ((phase === "failed" || phase === "live-after-failure") && latest) {
      return (
        <Row label="Last deploy">
          <span className="text-warning">
            {latest.phase === DeployPhase.ABANDONED
              ? "Abandoned "
              : latest.phase === DeployPhase.LOST
                ? "Lost "
                : "Failed "}
            {deployTime(toDate(latest.finishedAt ?? latest.requestedAt))}
            {latest.reason ? ` · ${latest.reason}` : ""}
          </span>
        </Row>
      );
    }
    const deploy = serving ?? latest;
    if (!deploy) {
      return <Row label="Last deploy">No deploys yet</Row>;
    }
    const duration = buildDurationLabel(deploy);
    const observed = duration?.startsWith("≈") === true;
    return (
      <Row label="Last deploy">
        <span className="inline-flex flex-col items-end">
          <span>
            {deployTime(toDate(deploy.liveAt ?? deploy.finishedAt))}
            {duration ? ` · ${duration}` : ""}
          </span>
          {observed ? (
            <span
              className="text-xs text-muted-foreground"
              title={observedTimesTitle}
            >
              observed by the control plane
            </span>
          ) : null}
        </span>
      </Row>
    );
  })();

  const httpsRow = (() => {
    if (!apex) {
      return null;
    }
    switch (apex.state) {
      case HostnameState.CERTIFICATE_READY:
        return (
          <Row label="HTTPS">
            <span className="text-success">On · auto-renewing</span>
          </Row>
        );
      case HostnameState.ROUTES_READY:
        return (
          <Row label="HTTPS">
            <span className="text-success">On · platform certificate</span>
          </Row>
        );
      case HostnameState.CERTIFICATE_PENDING:
        return (
          <Row label="HTTPS">
            <span className="text-warning">
              Pending ·{" "}
              {nameserverState !== undefined && nameserverState !== "pointed"
                ? "pending until the nameservers point here"
                : apex.reason || "the certificate hasn't been issued yet"}
            </span>
          </Row>
        );
      case HostnameState.UNROUTED:
        return (
          <Row label="HTTPS">
            <span className="text-warning">
              Not routed — the hosting engine has no gateway configured
            </span>
          </Row>
        );
      case HostnameState.REGISTERED:
        return (
          <Row label="HTTPS">
            <span className="text-muted-foreground">
              Requested automatically · status not reported by this hosting
              engine version
            </span>
          </Row>
        );
      case HostnameState.MISSING:
        return (
          <Row label="HTTPS">
            <span className="inline-flex flex-col items-end gap-1">
              <span className="text-warning">
                {apex.retrying
                  ? "Hostname not registered — re-registering automatically"
                  : `Hostname not registered${apex.reason ? ` — ${apex.reason}` : ""}`}
              </span>
              {apex.retrying && onReregister ? (
                <CardLink onClick={onReregister}>Re-register now</CardLink>
              ) : null}
            </span>
          </Row>
        );
      default:
        return (
          <Row label="HTTPS">
            <span className="text-muted-foreground">Not reported</span>
          </Row>
        );
    }
  })();

  // What the engine reports on the www host, not what the site asked for: an
  // engine that ignores `redirect_to` and one with nothing to redirect are
  // identical on the wire, so `capabilities.domain_redirect` — true only once
  // the engine has actually reported a redirect — separates the two.
  const wwwReading = wwwModeReading(
    zoneName,
    site,
    hostingStatus?.engine?.capabilities["domain_redirect"] === true,
  );

  const wwwRow = (
    <Row label={`www.${zoneName}`}>
      {site.www ? (
        <span className="inline-flex flex-col items-end gap-0.5">
          <span
            className={
              wwwReading.kind === "unreported" ? "text-muted-foreground" : ""
            }
          >
            {wwwReading.text}
          </span>
          {wwwReading.note ? (
            <span className="text-xs text-muted-foreground">
              {wwwReading.note}
            </span>
          ) : null}
        </span>
      ) : (
        <span className="text-muted-foreground">Not served (skipped)</span>
      )}
    </Row>
  );

  const dnsRow = (
    <Row label="DNS">
      {site.dns?.inSync ? (
        <span className="text-success">Automatic records in place</span>
      ) : (
        <span className="inline-flex flex-col items-end gap-1">
          <span className="text-warning">
            Drifted
            {site.dns?.problems.length ? ` · ${site.dns.problems[0]}` : ""}
          </span>
          {onReapplyDns ? (
            <CardLink onClick={onReapplyDns}>Re-apply</CardLink>
          ) : null}
        </span>
      )}
    </Row>
  );

  const platformAddressRow = site.autoHostname ? (
    <Row label="Platform address">
      <span className="inline-flex items-center gap-2">
        <span className="font-mono break-all">{site.autoHostname}</span>
        {onDeploy ? (
          <CopyButton value={site.autoHostname} label="Copy" />
        ) : null}
      </span>
    </Row>
  ) : null;

  /* ------------------------------------------------------------- preview -- */

  const preview = (() => {
    if (phase === "live" || phase === "live-after-failure") {
      const deploy = serving;
      const revision = deploy?.source?.revision;
      return (
        <div className={previewBoxClassName}>
          <span className="text-xs text-muted-foreground">
            {revision ? `${revision} · ` : ""}
            {deployTime(toDate(deploy?.liveAt ?? deploy?.finishedAt))}
          </span>
          <span className="max-w-full truncate font-mono text-[15px] text-foreground">
            {site.autoHostname || zoneName}
          </span>
        </div>
      );
    }
    const caption =
      phase === "attaching"
        ? "registering hosts"
        : phase === "detaching"
          ? "removing hosts"
          : phase === "zone-missing"
            ? "zone missing"
            : phase === "queued"
              ? "queued"
              : phase === "building"
                ? "building"
                : phase === "failed"
                  ? "not live"
                  : "attached · no deploy yet";
    return (
      <div className={previewBoxClassName}>
        <span className="text-xs text-muted-foreground">{caption}</span>
        <span className="max-w-full truncate font-mono text-[15px] text-foreground">
          {zoneName}
        </span>
      </div>
    );
  })();

  /* --------------------------------------------------------------- body --- */

  const body = (() => {
    if (phase === "attaching") {
      return (
        <>
          <p className="text-subtle leading-[1.55]">
            Registering {zoneName}
            {site.www ? ` and www.${zoneName}` : ""} with the hosting engine…
          </p>
          {site.hostnames.length ? (
            <div className="flex flex-col gap-3.5">
              {site.hostnames.map((hostname) => (
                <Row key={hostname.host} label={hostname.host}>
                  {hostname.state === HostnameState.MISSING ? (
                    <span className="text-muted-foreground">registering…</span>
                  ) : (
                    <span className="text-success">registered</span>
                  )}
                </Row>
              ))}
            </div>
          ) : null}
        </>
      );
    }
    if (phase === "detaching") {
      return (
        <p className="text-subtle leading-[1.55]">
          The hosting engine is removing the hosts…
        </p>
      );
    }
    if (phase === "zone-missing") {
      return (
        <p className="text-subtle leading-[1.55]">
          This domain&apos;s zone no longer exists (restored from an older
          snapshot?). Detach the site to release it.
        </p>
      );
    }
    return (
      <div className="flex flex-col gap-3.5">
        <Row label="Source">
          <DeploySourceValue deploy={shownDeploy} />
        </Row>
        {branchRow}
        {lastDeployRow}
        {phase === "live-after-failure" && serving ? (
          <Row label="Serving">
            <span className="inline-flex flex-col items-end">
              {serving.source?.revision ? (
                <span className="font-mono" title={servingCommit?.full}>
                  {serving.source.revision}
                  {servingCommit ? ` @ ${servingCommit.short}` : ""}
                </span>
              ) : null}
              <span className="text-muted-foreground">
                {deployTime(toDate(serving.liveAt ?? serving.finishedAt))}
              </span>
            </span>
          </Row>
        ) : null}
        {httpsRow}
        {wwwRow}
        {dnsRow}
        {platformAddressRow}
      </div>
    );
  })();

  const failureNote =
    phase === "failed"
      ? latest?.phase === DeployPhase.LOST
        ? "The hosting engine no longer reports this build."
        : latest?.reason ||
          "The hosting engine didn't say why the build stopped."
      : undefined;

  /* ------------------------------------------------------------- footer --- */

  // The design's Environment action, and the choice of what www does. Both are
  // offered wherever the site is settled enough to have settings at all; the www
  // link is hidden on a site that has no www host to talk about.
  const settingsLinks = (
    <>
      {onEnvironment ? (
        <CardLink onClick={onEnvironment}>Environment</CardLink>
      ) : null}
      {onWwwMode && site.www ? (
        <CardLink onClick={onWwwMode}>www settings</CardLink>
      ) : null}
    </>
  );

  const links = (() => {
    if (phase === "attaching" || phase === "detaching") {
      return null;
    }
    if (phase === "zone-missing") {
      return onDetach ? (
        <CardLink onClick={onDetach}>Detach…</CardLink>
      ) : null;
    }
    if (phase === "queued" || phase === "building") {
      // Settings stay reachable while a build runs: neither a variable nor the
      // www mode touches the build in flight, and a variable is most often set
      // while waiting for one.
      return (
        <>
          {onShowLog ? (
            <CardLink onClick={onShowLog}>Deploy log</CardLink>
          ) : null}
          {settingsLinks}
        </>
      );
    }
    if (phase === "no-deploy") {
      return (
        <>
          {onDeploy ? <CardLink onClick={onDeploy}>Deploy</CardLink> : null}
          {settingsLinks}
          {onDetach ? (
            <CardLink onClick={onDetach}>Detach site</CardLink>
          ) : null}
        </>
      );
    }
    if (phase === "failed") {
      return (
        <>
          {onShowLog ? (
            <CardLink onClick={onShowLog}>Deploy log</CardLink>
          ) : null}
          {onDeploy ? (
            <CardLink onClick={onDeploy}>Deploy again</CardLink>
          ) : null}
          {settingsLinks}
          {onDetach ? <CardLink onClick={onDetach}>Detach…</CardLink> : null}
        </>
      );
    }
    if (phase === "live-after-failure") {
      return (
        <>
          {onShowLog ? (
            <CardLink onClick={onShowLog}>Deploy log</CardLink>
          ) : null}
          {onDeploy ? (
            <CardLink onClick={onDeploy}>Deploy again</CardLink>
          ) : null}
          {onRollback ? (
            <CardLink onClick={onRollback}>Rollback</CardLink>
          ) : null}
          {settingsLinks}
          {onDetach ? <CardLink onClick={onDetach}>Detach…</CardLink> : null}
        </>
      );
    }
    return (
      <>
        {onDeploy ? <CardLink onClick={onDeploy}>Deploy</CardLink> : null}
        {onShowLog ? <CardLink onClick={onShowLog}>Deploy log</CardLink> : null}
        {onRollback ? (
          <CardLink onClick={onRollback}>Rollback</CardLink>
        ) : null}
        {settingsLinks}
        {onDetach ? <CardLink onClick={onDetach}>Detach…</CardLink> : null}
      </>
    );
  })();

  const degradedNote =
    phase !== "zone-missing" &&
    site.state === SiteState.DEGRADED &&
    site.reason
      ? site.reason
      : undefined;

  // The card is polled while a deploy runs, so ATTACHING -> BUILDING -> LIVE (or
  // FAILED) arrives with no action from the reader. A pill that silently swaps its
  // text and colour tells a screen-reader user nothing, so the same reading is kept
  // in a live region that is mounted for the life of the card: each new phase is
  // announced once, and the failure reason is announced with it.
  const spokenState = `${zoneName} website: ${pill.label.toLowerCase()}${
    failureNote ? `. ${failureNote}` : ""
  }${degradedNote ? `. ${degradedNote}` : ""}`;

  return (
    <Card>
      <span role="status" className="sr-only">
        {spokenState}
      </span>
      <CardHeader>
        <CardTitle>Website</CardTitle>
        <div className="flex shrink-0 flex-col items-end gap-1">
          <StatusPill
            tone={pill.tone}
            pulse={pill.pulse}
            aria-label={`Website ${pill.label}`}
          >
            {pill.label}
          </StatusPill>
          {phase === "live-after-failure" ? (
            <span className="text-xs text-warning">last deploy failed</span>
          ) : null}
        </div>
      </CardHeader>

      <CardContent>
        {preview}
        {body}
        {failureNote ? (
          <p className="text-xs text-warning leading-[1.55]">{failureNote}</p>
        ) : null}
        {degradedNote ? (
          <p className="text-xs text-warning leading-[1.55]">{degradedNote}</p>
        ) : null}
        {/* The records the zone still carries, for the reader who came for them. */}
        {website.apex.length || website.www.targets.length ? (
          <p className="text-xs text-muted-foreground leading-[1.55]">
            Website records in this zone are written and kept up to date by the
            hosting engine.
          </p>
        ) : null}
      </CardContent>

      {footer !== undefined || links ? (
        <CardFooter>{footer ?? links}</CardFooter>
      ) : null}
    </Card>
  );
}

/* -------------------------------------------------------------------------- */

/**
 * The website half of a domain: the zone's records when nothing is attached, and the
 * hosting engine's own view of the site once one is. Nothing here observes HTTP or
 * infers a state — every pill, row and sentence is a reading of `Site`, of the
 * hosting status, or of the zone itself.
 */
export function WebsiteCard(props: WebsiteCardProps) {
  const {
    zoneName,
    website,
    updatedLabel,
    onPoint,
    onShowRecords,
    footer,
    site,
    hostingConfigured = false,
    hostingStatus,
    uploadsEnabled = false,
    onAttach,
  } = props;

  if (site) {
    return <SiteCard {...props} />;
  }

  const records = {
    zoneName,
    website,
    updatedLabel,
    onPoint,
    onShowRecords,
    footer,
  };

  // Nothing is known about the hosting engine (the landing frame, or the status has
  // not loaded yet): the phase-1 card, verbatim.
  if (hostingStatus === undefined) {
    return <RecordsCard {...records} />;
  }

  if (!hostingConfigured) {
    return (
      <RecordsCard
        {...records}
        note="Hosting isn't configured — records only."
      />
    );
  }

  const hasRecords =
    website.apex.length > 0 || website.www.targets.length > 0;

  if (!hasRecords) {
    return (
      <RecordsCard
        {...records}
        setupParagraph={
          <p className="text-subtle leading-[1.55]">
            Nothing here yet. Connect a GitHub repository
            {uploadsEnabled ? " or drop a folder" : ""} and the site is built and
            served at {zoneName}; a certificate is issued automatically once the
            nameservers point here, and the DNS records are written for you.
          </p>
        }
        extraLinks={
          <>
            {onAttach ? (
              <CardLink onClick={onAttach}>Host it here</CardLink>
            ) : null}
            {onPoint ? (
              <CardLink onClick={onPoint}>Point elsewhere</CardLink>
            ) : null}
          </>
        }
        // The engine links above replace the phase-1 "Point website here" link.
        onPoint={undefined}
      />
    );
  }

  const gateway = hostingStatus.gatewayAddresses;
  const atGateway =
    website.apex.length > 0 &&
    gateway.length > 0 &&
    website.apex.every(
      (target) => target.type !== "CNAME" && gateway.includes(target.value),
    );

  return (
    <RecordsCard
      {...records}
      note={
        atGateway
          ? "Pointed at this platform's gateway but not attached — Attach site adopts these records."
          : "Pointed elsewhere. Host it here instead?"
      }
      extraLinks={
        onAttach ? (
          <CardLink onClick={onAttach}>
            {atGateway ? "Attach site" : "Host it here"}
          </CardLink>
        ) : null
      }
    />
  );
}
