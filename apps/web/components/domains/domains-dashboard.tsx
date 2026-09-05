"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useMemo, useState } from "react";
import { MoreHorizontal, TriangleAlert } from "lucide-react";

import { PageHeader } from "@/components/app/page-header";
import { usePlatform } from "@/components/app/platform-provider";
import { useZones } from "@/components/app/zones-provider";
import { DashboardEmptyState } from "@/components/domains/dashboard-empty-state";
import { DashboardSkeleton } from "@/components/domains/dashboard-skeleton";
import { DashboardUnavailable } from "@/components/domains/dashboard-unavailable";
import { DomainsTable } from "@/components/domains/domains-table";
import { SummaryCards } from "@/components/domains/summary-cards";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { ImportZoneDialog } from "@/components/zonefile/import-zone-dialog";
import { ZoneDialog } from "@/components/zone-dialog";
import { useNow } from "@/hooks/use-now";
import { derivePlatformDashboard } from "@/lib/dashboard-model";
import { lower, zoneHref } from "@/lib/dns-values";
import { plural } from "@/lib/format";
import { deriveDashboard } from "@/lib/zone-model";

type DashboardDialog = "zone" | "import";

/** "hosting", "hosting and mail", "hosting, mail and billing". */
function listOf(names: string[]): string {
  if (names.length <= 1) {
    return names[0] ?? "";
  }
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

export function DomainsDashboard() {
  const router = useRouter();
  const {
    zones,
    server,
    loaded,
    loadedAt,
    refreshing,
    error,
    refresh,
    createZone,
  } = useZones();
  const { hosting, mail, billing, status, statusError, refreshStatus } =
    usePlatform();
  const [query, setQuery] = useState("");
  const [dialog, setDialog] = useState<DashboardDialog>();
  const now = useNow(60_000);

  const zoneDashboard = useMemo(
    () => deriveDashboard(zones, server),
    [zones, server],
  );

  // The engine layer is folded in here rather than inside `deriveDashboard`, which stays
  // the pure zone-only model: an engine that is off or unreachable contributes nothing
  // and every cell keeps its phase-1 text.
  const dashboard = useMemo(
    () =>
      derivePlatformDashboard(
        zoneDashboard,
        hosting.items,
        mail.items,
        billing.items,
        {
          hosting: hosting.configured,
          mail: mail.configured,
          billing: billing.configured,
        },
        now,
        {
          hosting: hosting.status,
          mail: mail.status,
          billing: billing.status,
        },
      ),
    [
      billing.configured,
      billing.items,
      billing.status,
      hosting.configured,
      hosting.items,
      hosting.status,
      mail.configured,
      mail.items,
      mail.status,
      now,
      zoneDashboard,
    ],
  );

  // The three engines have their own error channels, and an engine that did not answer
  // contributes no attention item — so an empty attention list below is only "nothing
  // is wrong" once every configured engine has actually reported.
  const enginesUnknown = statusError !== undefined && status === undefined;
  const silentEngines = enginesUnknown
    ? []
    : ([
        hosting.configured && hosting.error ? "hosting" : undefined,
        mail.configured && mail.error ? "mail" : undefined,
        billing.configured && billing.error ? "billing" : undefined,
      ].filter(Boolean) as string[]);
  const noWarningsNote = enginesUnknown
    ? "No warnings from the DNS store; the control plane didn't report which engines it runs, so nothing was asked of them."
    : silentEngines.length
      ? `No warnings from the DNS store; the ${listOf(silentEngines)} ${plural(silentEngines.length, "engine")} didn't answer.`
      : undefined;

  // `loaded` only means the first listZones settled; it settles on failure too, with an
  // empty zone list. Until one list call has actually succeeded the zone list is unknown,
  // so nothing may claim "No domains yet" or summarise zero domains. Dismissing the error
  // alert does not make the list known, so this does not depend on `error`.
  const unknown = loaded && loadedAt === undefined;

  const needle = lower(query);
  const rows = useMemo(
    () =>
      needle
        ? dashboard.rows.filter((row) => row.name.includes(needle))
        : dashboard.rows,
    [dashboard.rows, needle],
  );

  async function handleCreateZone(name: string) {
    const zone = await createZone(name);
    router.push(zoneHref(zone.name));
  }

  return (
    <div className="flex flex-col gap-7">
      <PageHeader
        title="Domains"
        subtitle={
          unknown ? (
            "Domain list unavailable"
          ) : loaded ? (
            dashboard.subtitle
          ) : (
            <span
              aria-hidden="true"
              className="inline-block h-3.5 w-44 motion-safe:animate-pulse rounded bg-muted align-middle"
            />
          )
        }
        actions={
          <>
            <Input
              type="search"
              aria-label="Search domains"
              placeholder="Search"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              className="text-ui h-9 w-full rounded-[7px] placeholder:text-light md:w-[200px] md:text-ui"
            />
            <Button
              size="lg"
              className="text-ui rounded-[7px] px-3.5 font-semibold"
              asChild
            >
              <Link href="/connect">Connect a domain</Link>
            </Button>
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button
                  variant="outline"
                  size="icon-lg"
                  className="rounded-[7px]"
                  aria-label="More actions"
                >
                  <MoreHorizontal aria-hidden="true" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onSelect={() => setDialog("zone")}>
                  Add zone only
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setDialog("import")}>
                  Import a zone file…
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </>
        }
      />

      {!loaded ? (
        <DashboardSkeleton />
      ) : unknown ? (
        <DashboardUnavailable
          error={error}
          retrying={refreshing}
          onRetry={() => {
            void refresh();
          }}
        />
      ) : (
        <>
          {enginesUnknown ? (
            <Alert>
              <TriangleAlert aria-hidden="true" className="text-warning" />
              <AlertTitle>
                The control plane didn&rsquo;t report which engines it runs
              </AlertTitle>
              <AlertDescription>
                <div className="text-soft">
                  {statusError} The table below shows what the DNS store holds
                  and nothing from the engines, so a domain with a website or
                  mailboxes here reads as if it had neither.
                </div>
                <div className="mt-2">
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => {
                      void refreshStatus(true);
                    }}
                  >
                    Retry
                  </Button>
                </div>
              </AlertDescription>
            </Alert>
          ) : silentEngines.length ? (
            <p role="alert" className="text-ui text-warning">
              The {listOf(silentEngines)}{" "}
              {plural(silentEngines.length, "engine")} didn&rsquo;t answer, so
              the columns {silentEngines.length > 1 ? "they fill" : "it fills"}{" "}
              show DNS records only.{" "}
              <button
                type="button"
                className="link"
                onClick={() => {
                  void refreshStatus(true);
                }}
              >
                Retry
              </button>
            </p>
          ) : null}

          {zones.length === 0 ? (
            <DashboardEmptyState onAddZone={() => setDialog("zone")} />
          ) : (
            <DomainsTable
              rows={rows}
              query={query}
              billingColumn={billing.configured}
            />
          )}
          <SummaryCards
            attention={dashboard.attention}
            noWarningsNote={noWarningsNote}
            latest={zoneDashboard.latest}
            server={zoneDashboard.server}
            latestDeploy={dashboard.latestDeploy}
            mailTotals={dashboard.mailTotals}
            total={zones.length}
            now={now}
          />
        </>
      )}

      {/* Mounted conditionally so each dialog re-seeds its fields every time. */}
      {dialog === "zone" ? (
        <ZoneDialog
          open
          onOpenChange={(open) => setDialog(open ? "zone" : undefined)}
          onCreate={handleCreateZone}
        />
      ) : null}
      {dialog === "import" ? (
        <ImportZoneDialog
          open
          mode="create"
          onOpenChange={(open) => setDialog(open ? "import" : undefined)}
          onImported={() => setDialog(undefined)}
        />
      ) : null}
    </div>
  );
}
