"use client";

import { ArrowUpRight, Check, TriangleAlert } from "lucide-react";
import Link from "next/link";
import {
  useEffect,
  useId,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import { EngineUnreachable } from "@/components/app/engine-unreachable";
import { usePlatform } from "@/components/app/platform-provider";
import { RecordChangeList } from "@/components/app/record-change-list";
import { useZone, useZones } from "@/components/app/zones-provider";
import { DeleteRecordDialog } from "@/components/domain/delete-record-dialog";
import { DeleteZoneDialog } from "@/components/domain/delete-zone-dialog";
import { DnsCard } from "@/components/domain/dns-card";
import { DomainHeader } from "@/components/domain/domain-header";
import { DomainNotFound } from "@/components/domain/domain-not-found";
import { DomainSkeleton } from "@/components/domain/domain-skeleton";
import { DomainUnavailable } from "@/components/domain/domain-unavailable";
import { EmailCard } from "@/components/domain/email-card";
import { RawZoneTable } from "@/components/domain/raw-zone-table";
import { RecentActivity } from "@/components/domain/recent-activity";
import { RecordRowActions } from "@/components/domain/record-row-actions";
import { WebsiteCard } from "@/components/domain/website-card";
import { GuidedDialog } from "@/components/guided/guided-dialog";
import { VerifyServiceDialog } from "@/components/guided/verify-service-dialog";
import { AttachSiteDialog } from "@/components/hosting/attach-site-dialog";
import { DeployDialog } from "@/components/hosting/deploy-dialog";
import { DeployLogSheet } from "@/components/hosting/deploy-log-sheet";
import { DetachSiteDialog } from "@/components/hosting/detach-site-dialog";
import { RollbackDialog } from "@/components/hosting/rollback-dialog";
import { SiteEnvSheet } from "@/components/hosting/site-env-sheet";
import { WwwModeDialog } from "@/components/hosting/www-mode-dialog";
import { BindMailDialog } from "@/components/mail/bind-mail-dialog";
import { ForwarderDialog } from "@/components/mail/forwarder-dialog";
import { MailQueueSheet } from "@/components/mail/mail-queue-sheet";
import { MailboxCreatedDialog } from "@/components/mail/mailbox-created-dialog";
import { MailboxDialog } from "@/components/mail/mailbox-dialog";
import { UnbindMailDialog } from "@/components/mail/unbind-mail-dialog";
import { RecordDialog } from "@/components/record-dialog";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { ExportZoneDialog } from "@/components/zonefile/export-zone-dialog";
import { ImportZoneDialog } from "@/components/zonefile/import-zone-dialog";
import {
  SubscriptionState,
  type DomainBilling,
} from "@/gen/billing/v1/billing_pb";
import {
  RecordType,
  type Record as DNSRecord,
  type Zone,
} from "@/gen/dns/v1/dns_pb";
import { DeployPhase, HostnameState } from "@/gen/hosting/v1/hosting_pb";
import type { CreateMailboxResponse } from "@/gen/mail/v1/mail_pb";
import { EngineKind, type RecordChange } from "@/gen/platform/v1/platform_pb";
import { useDeployPolling } from "@/hooks/use-deploy-polling";
import { useNameserverCheck } from "@/hooks/use-nameserver-check";
import {
  useDeploys,
  useForwarders,
  useMailQueue,
  useMailboxes,
} from "@/hooks/use-platform-resources";
import { connectHref, dkimSelector, type RecordDraft } from "@/lib/dns-values";
import { describeError } from "@/lib/errors";
import { formatShortDate, toDate } from "@/lib/format";
import { getHostingClient, getMailClient } from "@/lib/platform-client";
import {
  apexHostname,
  derivePlatformAttention,
  mergeAttention,
  subscriptionLabel,
} from "@/lib/platform-model";
import { ttlFor } from "@/lib/record-plan";
import { deriveDomain, healthLine } from "@/lib/zone-model";

export type DomainViewProps = {
  zoneName: string;
};

type RecordField = "name" | "type" | "ttl" | "value";

type RecordEditor = {
  record?: DNSRecord;
  initial?: Partial<RecordDraft>;
  title?: string;
  description?: ReactNode;
  lockType?: boolean;
  namePlaceholder?: string;
  validate?: (
    draft: RecordDraft,
  ) => { field: RecordField; message: string } | undefined;
};

type DialogKind =
  | "website"
  | "email"
  | "verify"
  | "export"
  | "import"
  | "delete-zone";

/** The engine dialogs and sheets, each carrying only what its own props need. */
type EngineDialog =
  | { kind: "attach" }
  | { kind: "deploy" }
  | { kind: "log" }
  | { kind: "rollback" }
  | { kind: "detach" }
  | { kind: "reapply-site"; intent: "records" | "hosts" }
  | { kind: "environment" }
  | { kind: "www-mode" }
  | { kind: "bind" }
  | { kind: "mailbox" }
  | { kind: "mailbox-created"; result: CreateMailboxResponse }
  | { kind: "forwarder" }
  | { kind: "queue" }
  | { kind: "unbind" }
  | { kind: "reapply-mail" };

const rawZoneId = "raw-zone";
const storageKey = (zoneName: string) => `deephost:raw-zone:${zoneName}`;

function dkimValidate(draft: RecordDraft) {
  const name = draft.name.trim();
  if (!name.endsWith("._domainkey") || dkimSelector(name) === "") {
    return { field: "name" as const, message: "use selector._domainkey" };
  }
  if (!/^v=dkim1\b/i.test(draft.value.trim())) {
    return {
      field: "value" as const,
      message: "a DKIM key starts with v=DKIM1",
    };
  }
  return undefined;
}

export function DomainView({ zoneName }: DomainViewProps) {
  const { zone, loaded } = useZone(zoneName);
  const { loadedAt } = useZones();
  // Set while the delete dialog is working: the zone leaves the store before the
  // router transition to /domains commits, and "no domain called X here" must not
  // flash in that gap (it can last a full RSC round-trip).
  const [leaving, setLeaving] = useState(false);

  if (!loaded) {
    return <DomainSkeleton />;
  }
  if (!zone) {
    if (leaving) {
      return <DomainSkeleton />;
    }
    // `loaded` also becomes true when the first list *failed*, leaving no zones
    // at all — that says nothing about this domain, so don't claim it is missing.
    // Keyed on `loadedAt` rather than `error`: dismissing the alert clears the
    // error while the list is still unknown.
    if (loadedAt === undefined) {
      return <DomainUnavailable zoneName={zoneName} />;
    }
    return <DomainNotFound zoneName={zoneName} />;
  }
  // Keyed by name so the per-zone raw-zone preference is read again when the
  // user moves from one domain to another.
  return (
    <DomainDetail key={zone.name} zone={zone} onDeletingChange={setLeaving} />
  );
}

function DomainDetail({
  zone,
  onDeletingChange,
}: {
  zone: Zone;
  onDeletingChange: (deleting: boolean) => void;
}) {
  const api = useZones();
  const model = useMemo(() => deriveDomain(zone), [zone]);
  const visitTooltipId = useId();

  const platform = usePlatform();
  const {
    hosting,
    mail,
    billing,
    engine,
    status,
    statusError,
    refreshStatus,
    invalidate,
  } = platform;
  const site = hosting.items.get(zone.id);
  const mailDomain = mail.items.get(zone.id);
  const billingRow = billing.items.get(zone.id);

  // One poller per domain view: it stops as soon as the site settles.
  useDeployPolling(site);

  // The header sits above all three cards, so its verdict has to see all three.
  // model.attention only reads the zone's records: on its own it says "Nothing
  // needs attention" above a degraded Email card or a site the engine will not
  // route, which is the page contradicting itself.
  const health = useMemo(
    () =>
      healthLine(
        mergeAttention(
          model.attention,
          derivePlatformAttention(zone.name, site, mailDomain, billingRow),
        ),
      ),
    [model.attention, zone.name, site, mailDomain, billingRow],
  );

  const mailZoneId = mailDomain ? zone.id : "";
  const mailboxes = useMailboxes(mailZoneId);
  const forwarders = useForwarders(mailZoneId);
  const mailQueue = useMailQueue(mailZoneId);
  const deploys = useDeploys(zone.id);

  const [rawShown, setRawShown] = useState(() => {
    try {
      return localStorage.getItem(storageKey(zone.name)) !== "0";
    } catch {
      return true;
    }
  });
  const [highlightIds, setHighlightIds] = useState<string[]>([]);
  const [recordEditor, setRecordEditor] = useState<RecordEditor>();
  const [deleteTarget, setDeleteTarget] = useState<DNSRecord>();
  const [dialog, setDialog] = useState<DialogKind>();
  const [engineDialog, setEngineDialog] = useState<EngineDialog>();

  const check = useNameserverCheck({
    domain: zone.name,
    expected: zone.nameservers,
    enabled: true,
    poll: false,
  });

  // Scrolling only; the highlight is cleared from a timer callback, never with a
  // synchronous setState inside the effect body.
  useEffect(() => {
    if (!rawShown || highlightIds.length === 0) {
      return;
    }
    const reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    document.getElementById(rawZoneId)?.scrollIntoView({
      behavior: reduced ? "auto" : "smooth",
      block: "start",
    });
    const timer = setTimeout(() => setHighlightIds([]), 2000);
    return () => clearTimeout(timer);
  }, [rawShown, highlightIds]);

  function persistRaw(next: boolean) {
    setRawShown(next);
    try {
      localStorage.setItem(storageKey(zone.name), next ? "1" : "0");
    } catch {
      // A blocked storage is not worth surfacing: the toggle still works.
    }
  }

  function showRecords(ids: string[]) {
    persistRaw(true);
    setHighlightIds(ids);
  }

  function idsForSource(source: "website" | "email") {
    return model.rows
      .filter((row) => row.source === source)
      .map((row) => row.record.id);
  }

  async function handleRecordSubmit(draft: RecordDraft) {
    const record = recordEditor?.record;
    if (record) {
      await api.updateRecord(zone.id, record.id, draft);
      return;
    }
    await api.createRecord(zone.id, draft);
  }

  async function handleDeleteRecord() {
    if (!deleteTarget) {
      return;
    }
    await api.deleteRecord(zone.id, deleteTarget.id);
    setDeleteTarget(undefined);
  }

  function handleImported() {
    // A REPLACE import regenerates every record id, so nothing may keep one.
    setHighlightIds([]);
    setDeleteTarget(undefined);
    setRecordEditor(undefined);
    setDialog(undefined);
  }

  /**
   * After an engine mutation: the three engine lists and the zone list (engines write
   * records), then the page-level lists that stay on the same query. A list whose query
   * is disabled is left alone — its own effect refetches when it becomes enabled.
   */
  async function afterEngineChange() {
    await invalidate();
    const pending: Array<Promise<void>> = [];
    if (mailZoneId) {
      pending.push(mailboxes.refresh(), forwarders.refresh(), mailQueue.refresh());
    }
    if (hosting.configured) {
      pending.push(deploys.refresh());
    }
    await Promise.all(pending);
  }

  /**
   * After a detach or an unbind the binding is gone, so the mail lists must not be
   * asked for again: the provider refresh alone settles the page.
   */
  async function afterEngineRemoval() {
    await invalidate();
  }

  const nameserverState = check.comparison.state;
  const nameserverLine =
    nameserverState === "pointed" ? (
      <span className="inline-flex items-center gap-1.5">
        <Check aria-hidden="true" className="size-3.5 text-success" />
        Nameservers: pointed here
      </span>
    ) : nameserverState === "partial" || nameserverState === "elsewhere" ? (
      <span className="inline-flex flex-wrap items-center gap-x-2">
        <span className="text-warning">Nameservers: not pointed yet</span>
        <span aria-hidden="true">·</span>
        <Link href={connectHref(zone.name)} className="link">
          Finish pointing →
        </Link>
      </span>
    ) : undefined;

  /* ------------------------------------------------------------ header ---- */

  const apex = apexHostname(site);
  const apexServes =
    apex !== undefined &&
    (apex.state === HostnameState.CERTIFICATE_READY ||
      apex.state === HostnameState.ROUTES_READY ||
      apex.state === HostnameState.REGISTERED);

  const visit: { href?: string; suffix?: string; disabledReason?: string } =
    !site
      ? model.website.url
        ? { href: model.website.url }
        : { disabledReason: "No website records yet" }
      : !site.live
        ? { disabledReason: "Nothing is deployed yet" }
        : apexServes
          ? { href: `https://${zone.name}` }
          : site.autoHostname
            ? {
                href: `https://${site.autoHostname}`,
                suffix: "(platform address)",
              }
            : { disabledReason: "Certificate pending" };

  const billingLine =
    billing.configured && billing.loaded
      ? subscriptionSentence(billingRow)
      : undefined;

  const actions = (
    <>
      {visit.href ? (
        <Button
          variant="outline"
          size="lg"
          className="text-ui rounded-[7px] px-3.5"
          asChild
        >
          <a href={visit.href} target="_blank" rel="noopener noreferrer">
            Visit site
            {visit.suffix ? (
              <span className="text-muted-foreground">{visit.suffix}</span>
            ) : null}
            <ArrowUpRight aria-hidden="true" className="size-3.5" />
          </a>
        </Button>
      ) : (
        <Tooltip>
          <TooltipTrigger asChild>
            <span
              tabIndex={0}
              className="inline-flex rounded-[7px] focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
              aria-describedby={visitTooltipId}
            >
              <Button
                variant="outline"
                size="lg"
                className="text-ui rounded-[7px] px-3.5"
                disabled
              >
                Visit site
                <ArrowUpRight aria-hidden="true" className="size-3.5" />
              </Button>
            </span>
          </TooltipTrigger>
          <TooltipContent id={visitTooltipId}>
            {visit.disabledReason}
          </TooltipContent>
        </Tooltip>
      )}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="outline"
            size="lg"
            className="text-ui rounded-[7px] px-3.5"
          >
            Settings
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onSelect={() => setDialog("export")}>
            Export zone file
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => setDialog("import")}>
            Import zone file…
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem
            variant="destructive"
            onSelect={() => setDialog("delete-zone")}
          >
            Delete domain…
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </>
  );

  /* ------------------------------------------------------------- cards ---- */

  const hostingStatus = hosting.loaded ? hosting.status : undefined;
  const mailStatus = mail.loaded ? mail.status : undefined;
  // `GetPlatformStatus` itself failed and no status was ever received: every
  // `configured` flag is still its initial false, so the per-engine notes below are
  // dropped and the cards fall back to their records-only reading. That is the one
  // state the provider's contract wrote `EngineUnreachable` for (§8.1), and it is
  // reported once for the page rather than twice per engine.
  const enginesUnknown = statusError !== undefined && status === undefined;
  const hostingError =
    !enginesUnknown && hosting.configured ? hosting.error : undefined;
  const mailError = !enginesUnknown && mail.configured ? mail.error : undefined;
  const rollbackCandidates = deploys.items.filter(
    (deploy) =>
      deploy.phase === DeployPhase.SUPERSEDED && deploy.releaseId !== "",
  );
  const logDeploy = site?.latest ?? site?.live;
  const deleteDomainSupported =
    engine(EngineKind.HOSTING)?.capabilities["delete_domain"] === true;
  // Only ever true once the engine has actually reported a redirect on a host:
  // an engine without the field and one with nothing to redirect are identical
  // on the wire, so this is the only thing that separates them.
  const domainRedirectSupported =
    engine(EngineKind.HOSTING)?.capabilities["domain_redirect"] === true;
  const mailboxLimit = mailboxes.limit || status?.mailboxesPerDomain || 0;

  function closeEngineDialog(open: boolean) {
    if (!open) {
      setEngineDialog(undefined);
    }
  }

  return (
    <>
      <DomainHeader
        zoneName={zone.name}
        healthTone={health.tone}
        healthLabel={health.healthLabel}
        serial={model.serial}
        updatedAt={model.updatedAt}
        nameservers={model.dns.nameservers}
        nameserverLine={nameserverLine}
        websiteUrl={visit.href}
        billingLine={billingLine}
        actions={actions}
      />

      {enginesUnknown ? (
        <Alert>
          <TriangleAlert aria-hidden="true" className="text-warning" />
          <AlertTitle>
            The control plane didn&rsquo;t report which engines it runs
          </AlertTitle>
          <AlertDescription>
            <div className="text-soft">
              {statusError} The cards below show this domain&rsquo;s DNS records
              only; a website or mailboxes on this domain may be hosted here and
              simply unreported.
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
      ) : null}

      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        <div className="flex flex-col gap-4">
          <WebsiteCard
            zoneName={zone.name}
            website={model.website}
            site={site}
            hostingConfigured={hosting.configured}
            hostingStatus={hostingStatus}
            nameserverState={nameserverState}
            uploadsEnabled={hostingStatus?.uploadsEnabled ?? false}
            onPoint={() => setDialog("website")}
            onShowRecords={() => showRecords(idsForSource("website"))}
            onAttach={
              hostingStatus ? () => setEngineDialog({ kind: "attach" }) : undefined
            }
            onDeploy={site ? () => setEngineDialog({ kind: "deploy" }) : undefined}
            onShowLog={
              logDeploy ? () => setEngineDialog({ kind: "log" }) : undefined
            }
            onRollback={
              site && rollbackCandidates.length
                ? () => setEngineDialog({ kind: "rollback" })
                : undefined
            }
            onDetach={site ? () => setEngineDialog({ kind: "detach" }) : undefined}
            onReapplyDns={
              site
                ? () =>
                    setEngineDialog({
                      kind: "reapply-site",
                      intent: "records",
                    })
                : undefined
            }
            onReregister={
              site
                ? () =>
                    setEngineDialog({ kind: "reapply-site", intent: "hosts" })
                : undefined
            }
            onEnvironment={
              site ? () => setEngineDialog({ kind: "environment" }) : undefined
            }
            onWwwMode={
              site ? () => setEngineDialog({ kind: "www-mode" }) : undefined
            }
          />
          {hostingError ? (
            <EngineUnreachable
              engine="hosting"
              reason={hostingError}
              checkedAt={toDate(engine(EngineKind.HOSTING)?.checkedAt)}
              onRetry={() => {
                void refreshStatus(true);
              }}
            />
          ) : null}
        </div>

        <div className="flex flex-col gap-4">
          <EmailCard
            zoneName={zone.name}
            email={model.email}
            mailDomain={mailDomain}
            mailboxes={mailboxes.loaded ? mailboxes.items : undefined}
            forwarders={forwarders.loaded ? forwarders.items : undefined}
            queue={mailQueue.queue?.summary}
            mailConfigured={mail.configured}
            mailStatus={mailStatus}
            onRoute={() => setDialog("email")}
            onAddDkim={() =>
              setRecordEditor({
                initial: { type: RecordType.TXT, name: "" },
                namePlaceholder: "s1._domainkey",
                title: "Add a DKIM key",
                description:
                  "Name is the selector followed by ._domainkey (for example s1._domainkey); the value is the key your provider gave you, starting v=DKIM1.",
                lockType: true,
                validate: dkimValidate,
              })
            }
            onShowRecords={() => showRecords(idsForSource("email"))}
            onBind={
              mailStatus ? () => setEngineDialog({ kind: "bind" }) : undefined
            }
            onUnbind={
              mailDomain ? () => setEngineDialog({ kind: "unbind" }) : undefined
            }
            onAddMailbox={
              mailDomain ? () => setEngineDialog({ kind: "mailbox" }) : undefined
            }
            onAddForwarder={
              mailDomain
                ? () => setEngineDialog({ kind: "forwarder" })
                : undefined
            }
            onOpenQueue={
              mailDomain ? () => setEngineDialog({ kind: "queue" }) : undefined
            }
            onReapplyDns={
              mailDomain
                ? () => setEngineDialog({ kind: "reapply-mail" })
                : undefined
            }
          />
          {mailError ? (
            <EngineUnreachable
              engine="mail"
              reason={mailError}
              checkedAt={toDate(engine(EngineKind.MAIL)?.checkedAt)}
              onRetry={() => {
                void refreshStatus(true);
              }}
            />
          ) : null}
        </div>

        <DnsCard
          zoneName={zone.name}
          groups={model.groups}
          total={model.dns.total}
          rawShown={rawShown}
          nameserverState={nameserverState}
          engineManaged={site !== undefined || mailDomain !== undefined}
          onToggleRaw={() => persistRaw(!rawShown)}
          onAddRecord={() =>
            setRecordEditor({
              initial: {
                name: "@",
                type: RecordType.A,
                ttl: ttlFor(zone, "@", RecordType.A),
              },
            })
          }
          onVerify={() => setDialog("verify")}
          onGroupSelect={(group) => showRecords(group.recordIds)}
        />

        <RecentActivity zoneId={zone.id} zoneName={zone.name} />
      </div>

      {rawShown ? (
        <RawZoneTable
          id={rawZoneId}
          zoneName={zone.name}
          rows={model.dns.rows}
          highlightIds={highlightIds}
          onExport={() => setDialog("export")}
          onImport={() => setDialog("import")}
          renderActions={(row) => (
            <RecordRowActions
              recordId={row.record.id}
              zone={zone}
              onEdit={(record) => setRecordEditor({ record })}
              onDelete={(record) => setDeleteTarget(record)}
            />
          )}
        />
      ) : null}

      {recordEditor ? (
        <RecordDialog
          key={recordEditor.record?.id ?? "new"}
          open
          zoneName={zone.name}
          record={recordEditor.record}
          initial={recordEditor.initial}
          title={recordEditor.title}
          description={recordEditor.description}
          lockType={recordEditor.lockType}
          namePlaceholder={recordEditor.namePlaceholder}
          validate={recordEditor.validate}
          onOpenChange={(open) => {
            if (!open) {
              setRecordEditor(undefined);
            }
          }}
          onSubmit={handleRecordSubmit}
        />
      ) : null}

      {deleteTarget ? (
        <DeleteRecordDialog
          key={deleteTarget.id}
          record={deleteTarget}
          zoneName={zone.name}
          onConfirm={handleDeleteRecord}
          onOpenChange={(open) => {
            if (!open) {
              setDeleteTarget(undefined);
            }
          }}
        />
      ) : null}

      {dialog === "website" || dialog === "email" ? (
        <GuidedDialog
          kind={dialog}
          open
          zone={zone}
          onOpenChange={(open) => {
            if (!open) {
              setDialog(undefined);
            }
          }}
          onApplied={() => setDialog(undefined)}
        />
      ) : null}

      {dialog === "verify" ? (
        <VerifyServiceDialog
          open
          zone={zone}
          onOpenChange={(open) => {
            if (!open) {
              setDialog(undefined);
            }
          }}
          onApplied={() => setDialog(undefined)}
        />
      ) : null}

      {dialog === "export" ? (
        <ExportZoneDialog
          open
          zone={zone}
          onOpenChange={(open) => {
            if (!open) {
              setDialog(undefined);
            }
          }}
        />
      ) : null}

      {dialog === "import" ? (
        <ImportZoneDialog
          open
          mode="replace"
          zone={zone}
          onOpenChange={(open) => {
            if (!open) {
              setDialog(undefined);
            }
          }}
          onImported={handleImported}
        />
      ) : null}

      {dialog === "delete-zone" ? (
        <DeleteZoneDialog
          zone={zone}
          open
          onOpenChange={(open) => {
            if (!open) {
              setDialog(undefined);
            }
          }}
          onDeletingChange={onDeletingChange}
          onDeleted={() => setDialog(undefined)}
        />
      ) : null}

      {/* ------------------------------------------------- engine dialogs -- */}

      {engineDialog?.kind === "attach" && hostingStatus ? (
        <AttachSiteDialog
          zone={zone}
          hostingStatus={hostingStatus}
          open
          onOpenChange={closeEngineDialog}
          onDone={() => {
            void afterEngineChange();
            setEngineDialog(undefined);
          }}
        />
      ) : null}

      {engineDialog?.kind === "deploy" && site ? (
        <DeployDialog
          zone={zone}
          site={site}
          uploadsEnabled={hostingStatus?.uploadsEnabled ?? false}
          uploadMaxBytes={Number(hostingStatus?.uploadMaxBytes ?? BigInt(0))}
          open
          onOpenChange={closeEngineDialog}
          onDone={() => {
            void afterEngineChange();
            setEngineDialog(undefined);
          }}
        />
      ) : null}

      {engineDialog?.kind === "log" && logDeploy ? (
        <DeployLogSheet
          zone={zone}
          deploy={logDeploy}
          buildDeadlineSeconds={hostingStatus?.buildDeadlineSeconds ?? 0}
          open
          onOpenChange={closeEngineDialog}
        />
      ) : null}

      {engineDialog?.kind === "rollback" && site ? (
        <RollbackDialog
          zone={zone}
          site={site}
          deploys={rollbackCandidates}
          open
          onOpenChange={closeEngineDialog}
          onDone={() => {
            void afterEngineChange();
            setEngineDialog(undefined);
          }}
        />
      ) : null}

      {engineDialog?.kind === "detach" && site ? (
        <DetachSiteDialog
          zone={zone}
          site={site}
          deleteDomainSupported={deleteDomainSupported}
          open
          onOpenChange={closeEngineDialog}
          onDone={() => {
            void afterEngineRemoval();
            setEngineDialog(undefined);
          }}
        />
      ) : null}

      {engineDialog?.kind === "environment" && site ? (
        <SiteEnvSheet zone={zone} open onOpenChange={closeEngineDialog} />
      ) : null}

      {engineDialog?.kind === "www-mode" && site ? (
        <WwwModeDialog
          zone={zone}
          site={site}
          domainRedirectSupported={domainRedirectSupported}
          open
          onOpenChange={closeEngineDialog}
          onDone={() => {
            void afterEngineChange();
            setEngineDialog(undefined);
          }}
        />
      ) : null}

      {engineDialog?.kind === "reapply-site" ? (
        <ReapplyDnsDialog
          zone={zone}
          engine="hosting"
          intent={engineDialog.intent}
          onClose={closeEngineDialog}
          onApplied={() => {
            void afterEngineChange();
            setEngineDialog(undefined);
          }}
        />
      ) : null}

      {engineDialog?.kind === "reapply-mail" ? (
        <ReapplyDnsDialog
          zone={zone}
          engine="mail"
          intent="records"
          onClose={closeEngineDialog}
          onApplied={() => {
            void afterEngineChange();
            setEngineDialog(undefined);
          }}
        />
      ) : null}

      {engineDialog?.kind === "bind" && mailStatus ? (
        <BindMailDialog
          zone={zone}
          mailStatus={mailStatus}
          open
          onOpenChange={closeEngineDialog}
          onDone={() => {
            void afterEngineChange();
            setEngineDialog(undefined);
          }}
        />
      ) : null}

      {engineDialog?.kind === "mailbox" ? (
        <MailboxDialog
          zone={zone}
          mode="create"
          limit={mailboxLimit}
          count={mailDomain?.mailboxCount ?? mailboxes.items.length}
          open
          onOpenChange={closeEngineDialog}
          onDone={(result) => {
            void afterEngineChange();
            // The one-time password lives only in this result and in the dialog
            // it is handed to; nothing else ever sees it.
            if ("password" in result && result.password) {
              setEngineDialog({ kind: "mailbox-created", result });
              return;
            }
            setEngineDialog(undefined);
          }}
        />
      ) : null}

      {engineDialog?.kind === "mailbox-created" ? (
        <MailboxCreatedDialog
          address={engineDialog.result.mailbox?.address ?? ""}
          password={engineDialog.result.password}
          retrievalProtocol={engineDialog.result.retrievalProtocol}
          retrievalHost={engineDialog.result.retrievalHost}
          retrievalPort={engineDialog.result.retrievalPort}
          smtpHost={engineDialog.result.smtpHost}
          smtpPort={engineDialog.result.smtpPort}
          open
          onOpenChange={closeEngineDialog}
        />
      ) : null}

      {engineDialog?.kind === "forwarder" ? (
        <ForwarderDialog
          zone={zone}
          mailboxes={mailboxes.items}
          open
          onOpenChange={closeEngineDialog}
          onDone={() => {
            void afterEngineChange();
            setEngineDialog(undefined);
          }}
        />
      ) : null}

      {engineDialog?.kind === "queue" ? (
        <MailQueueSheet
          zoneId={zone.id}
          open
          onOpenChange={closeEngineDialog}
        />
      ) : null}

      {engineDialog?.kind === "unbind" && mailDomain ? (
        <UnbindMailDialog
          zone={zone}
          mailDomain={mailDomain}
          mailboxes={mailboxes.items}
          forwarders={forwarders.items}
          open
          onOpenChange={closeEngineDialog}
          onDone={() => {
            void afterEngineRemoval();
            setEngineDialog(undefined);
          }}
        />
      ) : null}
    </>
  );
}

/* -------------------------------------------------------------------------- */
/* Subscription line                                                          */
/* -------------------------------------------------------------------------- */

/**
 * The header's subscription line. Rendered only when a billing engine is configured:
 * a console without one says nothing about subscriptions at all.
 */
function subscriptionSentence(row?: DomainBilling): ReactNode {
  const value = row?.state ?? SubscriptionState.UNBILLED;
  if (value === SubscriptionState.UNBILLED) {
    return (
      <span className="text-muted-foreground">
        Subscription: none ·{" "}
        <Link href="/billing" className="link">
          Billing →
        </Link>
      </span>
    );
  }
  const label = subscriptionLabel(value).toLowerCase();
  const renews =
    value === SubscriptionState.ACTIVE || value === SubscriptionState.TRIALING
      ? toDate(row?.currentPeriodEnd)
      : undefined;
  const cancels =
    value === SubscriptionState.CANCELING ? toDate(row?.cancelAt) : undefined;
  return (
    <span>
      Subscription:{" "}
      <Link href="/billing" className="link">
        {label}
        {renews ? ` · renews ${formatShortDate(renews)}` : ""}
        {cancels ? ` · cancels ${formatShortDate(cancels)}` : ""}
      </Link>
    </span>
  );
}

/* -------------------------------------------------------------------------- */
/* Re-apply engine records                                                    */
/* -------------------------------------------------------------------------- */

type ReapplyState = {
  loading: boolean;
  changes: RecordChange[];
  conflicts: RecordChange[];
  error?: string;
};

/**
 * The confirmation in front of `ReapplySiteDns` / `ReapplyMailDns`: a dry run first, so
 * the exact plan the control plane computed is on screen before anything is written,
 * and an explicit checkbox before any custom record is replaced.
 *
 * The same call re-registers the site's hosts with the hosting engine, which is what
 * the Website card's "Re-register now" link asks for — hence the `intent` wording.
 */
function ReapplyDnsDialog({
  zone,
  engine,
  intent,
  onClose,
  onApplied,
}: {
  zone: Zone;
  engine: "hosting" | "mail";
  intent: "records" | "hosts";
  onClose: (open: boolean) => void;
  onApplied: () => void;
}) {
  const [plan, setPlan] = useState<ReapplyState>({
    loading: true,
    changes: [],
    conflicts: [],
  });
  const [replace, setReplace] = useState(false);
  const [applying, setApplying] = useState(false);
  const [error, setError] = useState<string>();
  const checkboxId = useId();

  const zoneId = zone.id;
  useEffect(() => {
    let cancelled = false;
    // Every setState below runs in a promise continuation, never synchronously in
    // the effect body (the React Compiler lint rule).
    void (async () => {
      try {
        const response =
          engine === "hosting"
            ? await getHostingClient().reapplySiteDns({ zoneId, dryRun: true })
            : await getMailClient().reapplyMailDns({ zoneId, dryRun: true });
        if (cancelled) {
          return;
        }
        setPlan({
          loading: false,
          changes: response.dnsPlan,
          conflicts: response.conflicts,
        });
      } catch (caught) {
        if (cancelled) {
          return;
        }
        setPlan({
          loading: false,
          changes: [],
          conflicts: [],
          error: describeError(caught),
        });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [engine, zoneId]);

  async function apply() {
    setError(undefined);
    setApplying(true);
    try {
      if (engine === "hosting") {
        await getHostingClient().reapplySiteDns({
          zoneId,
          replaceConflictingRecords: replace,
          dryRun: false,
        });
      } else {
        await getMailClient().reapplyMailDns({
          zoneId,
          replaceConflictingRecords: replace,
          dryRun: false,
        });
      }
      onApplied();
    } catch (caught) {
      setError(describeError(caught));
      setApplying(false);
    }
  }

  const title =
    intent === "hosts"
      ? `Re-register ${zone.name} with the hosting engine?`
      : engine === "hosting"
        ? `Re-apply the website records for ${zone.name}?`
        : `Re-apply the email records for ${zone.name}?`;
  const blocked = plan.conflicts.length > 0 && !replace;

  return (
    <AlertDialog
      open
      onOpenChange={(next) => {
        if (!next && applying) {
          return;
        }
        onClose(next);
      }}
    >
      <AlertDialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto">
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription>
            {intent === "hosts"
              ? "The hosting engine is asked to register the hosts again and the automatic records are written back exactly as planned below."
              : "The automatic records below are written back into this zone. Nothing else in the zone is touched."}
          </AlertDialogDescription>
        </AlertDialogHeader>

        {plan.loading ? (
          <div className="flex flex-col gap-2">
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-4 w-4/5" />
          </div>
        ) : plan.error ? (
          <p role="alert" className="text-xs text-destructive">
            {plan.error}
          </p>
        ) : (
          <RecordChangeList changes={plan.changes} conflicts={plan.conflicts} />
        )}

        {plan.conflicts.length ? (
          <div className="flex items-start gap-2.5">
            <Checkbox
              id={checkboxId}
              checked={replace}
              onCheckedChange={(next) => setReplace(next === true)}
              disabled={applying}
            />
            <Label htmlFor={checkboxId} className="text-ui leading-[1.45]">
              Replace the {plan.conflicts.length} custom{" "}
              {plan.conflicts.length === 1 ? "record" : "records"} listed above
            </Label>
          </div>
        ) : null}

        {error ? (
          <p role="alert" className="text-xs text-destructive">
            {error}
          </p>
        ) : null}

        <AlertDialogFooter>
          <AlertDialogCancel disabled={applying}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={applying || plan.loading || blocked}
            onClick={(event) => {
              event.preventDefault();
              void apply();
            }}
          >
            {applying
              ? "Applying…"
              : intent === "hosts"
                ? "Re-register"
                : "Re-apply"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
