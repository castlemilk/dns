"use client";

import Link from "next/link";
import { useEffect, useMemo, useRef, useState } from "react";

import { EngineNotConfigured } from "@/components/app/engine-not-configured";
import { EngineUnreachable } from "@/components/app/engine-unreachable";
import { PageHeader } from "@/components/app/page-header";
import { usePlatform } from "@/components/app/platform-provider";
import { useZones } from "@/components/app/zones-provider";
import { DeploysTable } from "@/components/deploys/deploys-table";
import { DeployDialog } from "@/components/hosting/deploy-dialog";
import { SiteEnvPanel } from "@/components/hosting/site-env-panel";
import { DeployLogSheet } from "@/components/hosting/deploy-log-sheet";
import { RollbackDialog } from "@/components/hosting/rollback-dialog";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusPill } from "@/components/ui/status-pill";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import type { Deploy, Site } from "@/gen/hosting/v1/hosting_pb";
import { EngineKind } from "@/gen/platform/v1/platform_pb";
import { useDeployPolling } from "@/hooks/use-deploy-polling";
import { useDeploys } from "@/hooks/use-platform-resources";
import { zoneHref } from "@/lib/dns-values";
import { plural } from "@/lib/format";
import {
  deployPhaseLabel,
  isDeployActive,
  siteStateLabel,
  siteStateTone,
} from "@/lib/platform-model";

export type DeploysPageProps = {
  /** `?domain=` from the server component; preselects the domain filter. */
  initialDomain?: string;
};

const allDomains = "__all__";

/**
 * Every deploy this control plane recorded, and the sites it can start a new one for.
 * The three states below are final (spec2 §8.1, §12.1) — unreachable control API,
 * loading, and no hosting engine configured. Only the last one says "isn't configured".
 */
export function DeploysPage({ initialDomain }: DeploysPageProps) {
  const { hosting, engine, status, statusError, refreshStatus } = usePlatform();

  const sites = useMemo(
    () =>
      [...hosting.items.values()].sort((a, b) =>
        a.zoneName.localeCompare(b.zoneName),
      ),
    [hosting.items],
  );
  const running = hosting.status?.activeDeploys ?? 0;
  const showCounts = hosting.loaded && hosting.configured;

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Deploys"
        subtitle={
          showCounts
            ? `${sites.length} ${plural(sites.length, "site")} · ${running} running`
            : undefined
        }
      />
      {statusError && !status ? (
        // The control API did not answer: nothing is known about the engine, so the
        // page says so instead of claiming it "isn't configured" (spec2 §8.1).
        <EngineUnreachable
          engine="hosting"
          reason={statusError}
          onRetry={() => {
            void refreshStatus(true);
          }}
        />
      ) : !hosting.loaded ? (
        <PageSkeleton />
      ) : !hosting.configured ? (
        <EngineNotConfigured
          engine="hosting"
          missingEnv={engine(EngineKind.HOSTING)?.missingEnv ?? []}
        />
      ) : (
        <DeploysBody initialDomain={initialDomain} sites={sites} />
      )}
    </div>
  );
}

function PageSkeleton() {
  return (
    <div
      data-slot="deploys-skeleton"
      role="status"
      aria-busy="true"
      className="flex flex-col gap-3"
    >
      <span className="sr-only">Loading deploys…</span>
      <Skeleton className="h-10 w-full" />
      <Skeleton className="h-10 w-full" />
      <Skeleton className="h-10 w-full" />
    </div>
  );
}

/** Follows one site while a deploy of it is running; renders nothing. */
function SiteWatcher({ site }: { site: Site }) {
  useDeployPolling(site);
  return null;
}

type Sheet =
  | { kind: "deploy"; zone: Zone; site: Site }
  // The log sheet keeps the deploy *id*, not the object: `deploys.refresh()` replaces
  // every Deploy in the list, and a captured object would freeze the sheet's phase
  // pill and its live region on the reading they had when it was opened. `captured`
  // is only the fallback for a deploy the refreshed list no longer carries.
  | { kind: "log"; zone: Zone; deployId: string; captured: Deploy }
  | { kind: "rollback"; zone: Zone; site: Site; deploys: Deploy[] };

function DeploysBody({
  initialDomain,
  sites,
}: {
  initialDomain?: string;
  sites: Site[];
}) {
  const { hosting } = usePlatform();
  const { zones } = useZones();
  const [domain, setDomain] = useState(initialDomain ?? "");
  const [sheet, setSheet] = useState<Sheet>();

  const zoneByName = useMemo(
    () => new Map(zones.map((zone) => [zone.name, zone])),
    [zones],
  );
  // Every kebab action needs the Zone object, so a deploy whose domain is not in the
  // zone list (the first ListZones failed, or the domain was deleted after the build)
  // has no working action at all — the table disables them and says why.
  const knownZoneNames = useMemo(
    () => new Set(zones.map((zone) => zone.name)),
    [zones],
  );
  const selectedZone = domain ? zoneByName.get(domain) : undefined;
  const deploys = useDeploys(selectedZone?.id);
  const attachedZoneIds = useMemo(
    () => new Set(sites.map((site) => site.zoneId)),
    [sites],
  );

  // A site whose deploy phase moved is the signal that the list is stale: the site
  // objects are polled by `useDeployPolling`, the table follows them.
  const signature = sites
    .map((site) => `${site.zoneId}:${site.state}:${site.latest?.id ?? ""}:${site.latest?.phase ?? 0}`)
    .join("|");
  const lastSignature = useRef(signature);
  const refreshDeploys = deploys.refresh;
  useEffect(() => {
    if (lastSignature.current === signature) {
      return;
    }
    lastSignature.current = signature;
    void refreshDeploys();
  }, [refreshDeploys, signature]);

  const status = hosting.status;
  const uploadsEnabled = status?.uploadsEnabled ?? false;
  const shownSites = selectedZone
    ? sites.filter((site) => site.zoneId === selectedZone.id)
    : sites;
  const envSite = shownSites.length === 1 ? shownSites[0] : undefined;

  // Every site on this page is polled while it builds, so the table's pills change with
  // the reader's hands off the keyboard. This carries the same reading the pills do —
  // one line per site whose deploy is not finished — so a screen-reader user hears a
  // build start, and hears it finish.
  const running = sites
    .filter((site) => site.latest && isDeployActive(site.latest))
    .map((site) => `${site.zoneName}: ${deployPhaseLabel(site.latest!.phase).toLowerCase()}`);

  return (
    <div className="flex flex-col gap-6">
      <span role="status" className="sr-only">
        {running.length
          ? `${running.length} ${plural(running.length, "deploy")} running — ${running.join("; ")}.`
          : "No deploys running."}
      </span>

      {sites.map((site) => (
        <SiteWatcher key={site.zoneId} site={site} />
      ))}

      {hosting.error ? (
        <p role="alert" className="text-xs text-destructive">
          {hosting.error}
        </p>
      ) : null}

      {sites.length > 1 ? (
        <div className="flex flex-wrap items-center gap-2.5">
          <Select
            value={domain || allDomains}
            onValueChange={(value) =>
              setDomain(value === allDomains ? "" : value)
            }
          >
            <SelectTrigger aria-label="Filter by domain" className="w-56">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={allDomains}>All domains</SelectItem>
              {sites.map((site) => (
                <SelectItem key={site.zoneId} value={site.zoneName}>
                  {site.zoneName}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      ) : null}

      {shownSites.length ? (
        <ul className="flex flex-col gap-2">
          {shownSites.map((site) => {
            const zone = zoneByName.get(site.zoneName);
            const hosts = site.hostnames.map((hostname) => hostname.host);
            return (
              <li
                key={site.zoneId}
                className="flex flex-wrap items-center gap-x-3 gap-y-2 rounded-[10px] border border-line px-4 py-3"
              >
                <Link href={zoneHref(site.zoneName)} className="link text-sm font-medium">
                  {site.zoneName}
                </Link>
                <StatusPill
                  tone={siteStateTone(site.state)}
                  aria-label={`Site ${siteStateLabel(site.state)}`}
                >
                  {siteStateLabel(site.state)}
                </StatusPill>
                {hosts.length ? (
                  <span className="font-mono text-[12.5px] break-all text-muted-foreground">
                    {hosts.join(" · ")}
                  </span>
                ) : null}
                {zone ? (
                  <Button
                    size="sm"
                    variant="outline"
                    className="ml-auto"
                    onClick={() => setSheet({ kind: "deploy", zone, site })}
                  >
                    Deploy
                  </Button>
                ) : null}
              </li>
            );
          })}
        </ul>
      ) : null}

      {/* The site view: with one site in front of the reader, its runtime
          configuration belongs here rather than behind another page. With
          several, the domain filter above narrows to one first. */}
      {envSite ? (
        <section
          aria-labelledby="deploys-env-heading"
          className="flex flex-col gap-3.5 rounded-[10px] border border-line px-5 py-4"
        >
          <h2 id="deploys-env-heading" className="text-[15px] font-semibold">
            Environment variables · {envSite.zoneName}
          </h2>
          <SiteEnvPanel
            key={envSite.zoneId}
            zoneId={envSite.zoneId}
            zoneName={envSite.zoneName}
            intro
          />
        </section>
      ) : null}

      {!deploys.loaded ? (
        <PageSkeleton />
      ) : deploys.error ? (
        <p role="alert" className="text-xs text-destructive">
          {deploys.error}
        </p>
      ) : deploys.items.length === 0 ? (
        <div className="text-ui rounded-[10px] border border-line px-5 py-8 text-muted-foreground">
          No deploys yet — attach a site on a domain page and deploy from GitHub
          {uploadsEnabled ? " or a folder" : ""}.
        </div>
      ) : (
        <div className="flex flex-col gap-3">
          <DeploysTable
            deploys={deploys.items}
            attachedZoneIds={attachedZoneIds}
            knownZoneNames={knownZoneNames}
            onShowLog={(deploy) => {
              const zone = zoneByName.get(deploy.zoneName);
              if (zone) {
                setSheet({
                  kind: "log",
                  zone,
                  deployId: deploy.id,
                  captured: deploy,
                });
              }
            }}
            onRollback={(deploy) => {
              const zone = zoneByName.get(deploy.zoneName);
              const site = sites.find((entry) => entry.zoneId === deploy.zoneId);
              if (zone && site) {
                setSheet({ kind: "rollback", zone, site, deploys: [deploy] });
              }
            }}
            onDeployAgain={(deploy) => {
              const zone = zoneByName.get(deploy.zoneName);
              const site = sites.find((entry) => entry.zoneId === deploy.zoneId);
              if (zone && site) {
                setSheet({ kind: "deploy", zone, site });
              }
            }}
          />
          {deploys.nextCursor ? (
            <Button
              variant="outline"
              size="sm"
              className="self-start"
              onClick={() => {
                void deploys.loadMore();
              }}
            >
              Older
            </Button>
          ) : null}
        </div>
      )}

      {sheet?.kind === "deploy" ? (
        <DeployDialog
          zone={sheet.zone}
          site={sheet.site}
          uploadsEnabled={uploadsEnabled}
          uploadMaxBytes={Number(status?.uploadMaxBytes ?? BigInt(0))}
          open
          onOpenChange={(next) => {
            if (!next) {
              setSheet(undefined);
            }
          }}
          onDone={() => {
            void deploys.refresh();
          }}
        />
      ) : null}

      {sheet?.kind === "log" ? (
        <DeployLogSheet
          zone={sheet.zone}
          deploy={
            deploys.items.find((entry) => entry.id === sheet.deployId) ??
            sheet.captured
          }
          buildDeadlineSeconds={status?.buildDeadlineSeconds ?? 0}
          open
          onOpenChange={(next) => {
            if (!next) {
              setSheet(undefined);
            }
          }}
        />
      ) : null}

      {sheet?.kind === "rollback" ? (
        <RollbackDialog
          zone={sheet.zone}
          site={sheet.site}
          deploys={sheet.deploys}
          open
          onOpenChange={(next) => {
            if (!next) {
              setSheet(undefined);
            }
          }}
          onDone={() => {
            void deploys.refresh();
          }}
        />
      ) : null}
    </div>
  );
}
