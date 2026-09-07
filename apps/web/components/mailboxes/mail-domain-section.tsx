"use client";

import Link from "next/link";
import { MoreVertical } from "lucide-react";
import { useState } from "react";

import { RelativeTime } from "@/components/app/relative-time";
import { usePlatform } from "@/components/app/platform-provider";
import { ForwarderDialog } from "@/components/mail/forwarder-dialog";
import { MailboxCreatedDialog } from "@/components/mail/mailbox-created-dialog";
import { MailboxDialog } from "@/components/mail/mailbox-dialog";
import { ResetPasswordDialog } from "@/components/mail/reset-password-dialog";
import { DeleteForwarderDialog } from "@/components/mailboxes/delete-forwarder-dialog";
import { DeleteMailboxDialog } from "@/components/mailboxes/delete-mailbox-dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Label } from "@/components/ui/label";
import { Progress } from "@/components/ui/progress";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusDot } from "@/components/ui/status-dot";
import { StatusPill } from "@/components/ui/status-pill";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import {
  ForwarderKind,
  MailDomainState,
  type Forwarder,
  type MailDomain,
  type Mailbox,
} from "@/gen/mail/v1/mail_pb";
import { useForwarders, useMailboxes } from "@/hooks/use-platform-resources";
import { zoneHref } from "@/lib/dns-values";
import { describeError } from "@/lib/errors";
import { toDate } from "@/lib/format";
import { getMailClient } from "@/lib/platform-client";
import { formatBytes } from "@/lib/platform-model";
import { cn } from "@/lib/utils";

export type MailDomainSectionProps = {
  domain: MailDomain;
  /** The zone behind this binding; absent when the zone was deleted. */
  zone?: Zone;
  mailboxLimit: number;
};

const mailboxColumns = "md:grid-cols-[1.4fr_1fr_1.2fr_120px_44px]";
const mailboxHeaderColumns = "grid-cols-[1.4fr_1fr_1.2fr_120px_44px]";
const forwarderColumns = "md:grid-cols-[1.2fr_1.6fr_120px_80px]";
const forwarderHeaderColumns = "grid-cols-[1.2fr_1.6fr_120px_80px]";

const stateLabel: Record<number, string> = {
  [MailDomainState.BINDING]: "Binding",
  [MailDomainState.BOUND]: "Hosted",
  [MailDomainState.DEGRADED]: "Degraded",
  [MailDomainState.UNBINDING]: "Unbinding",
};

type Sheet =
  | { kind: "add-mailbox" }
  | { kind: "edit-mailbox"; mailbox: Mailbox }
  | { kind: "reset"; mailbox: Mailbox }
  | { kind: "delete-mailbox"; mailbox: Mailbox }
  | { kind: "add-forwarder" }
  | { kind: "delete-forwarder"; forwarder: Forwarder }
  | {
      kind: "created";
      address: string;
      password: string;
      retrievalProtocol: string;
      retrievalHost: string;
      retrievalPort: number;
      smtpHost: string;
      smtpPort: number;
    };

/** Mailboxes, forwarders and the automatic records for one bound domain. */
export function MailDomainSection({
  domain,
  zone,
  mailboxLimit,
}: MailDomainSectionProps) {
  const { invalidate } = usePlatform();
  const mailboxes = useMailboxes(domain.zoneId);
  const forwarders = useForwarders(domain.zoneId);
  const [sheet, setSheet] = useState<Sheet>();
  const [reapplying, setReapplying] = useState(false);
  const [reapplyError, setReapplyError] = useState<string>();
  const [autoconfigBusy, setAutoconfigBusy] = useState(false);
  const [autoconfigError, setAutoconfigError] = useState<string>();

  const limit = mailboxes.limit || mailboxLimit;
  const tone =
    domain.state === MailDomainState.BOUND && domain.recordsInSync
      ? "success"
      : domain.state === MailDomainState.DEGRADED || !domain.recordsInSync
        ? "warning"
        : "info";

  /**
   * The client-autoconfiguration records are on for a binding made since they existed
   * and off for every older one, so this is the only way to turn them on for a domain
   * that was bound before. `publish_client_autoconfig` is optional on the wire and
   * absent means unchanged — it is sent here, and only here, deliberately.
   */
  async function setClientAutoconfig(next: boolean) {
    setAutoconfigError(undefined);
    setAutoconfigBusy(true);
    try {
      await getMailClient().updateMailDomain({
        zoneId: domain.zoneId,
        publishClientAutoconfig: next,
      });
      await invalidate();
    } catch (caught) {
      setAutoconfigError(describeError(caught));
    } finally {
      setAutoconfigBusy(false);
    }
  }

  async function reapply() {
    setReapplyError(undefined);
    setReapplying(true);
    try {
      await getMailClient().reapplyMailDns({
        zoneId: domain.zoneId,
        replaceConflictingRecords: false,
        dryRun: false,
      });
      await invalidate();
    } catch (caught) {
      setReapplyError(describeError(caught));
    } finally {
      setReapplying(false);
    }
  }

  return (
    <section className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <h2 className="text-[15px] font-semibold">
          {zone ? (
            <Link href={zoneHref(domain.zoneName)} className="link">
              {domain.zoneName}
            </Link>
          ) : (
            domain.zoneName
          )}
        </h2>
        <StatusPill
          tone={tone}
          pulse={
            domain.state === MailDomainState.BINDING ||
            domain.state === MailDomainState.UNBINDING
          }
          aria-label={`Mail ${stateLabel[domain.state] ?? "Unknown"}`}
        >
          {stateLabel[domain.state] ?? "Unknown"}
        </StatusPill>
        {domain.zoneMissing ? (
          <span className="text-xs text-warning">
            this domain&apos;s zone no longer exists
          </span>
        ) : domain.reason ? (
          <span className="text-xs text-warning">{domain.reason}</span>
        ) : null}
        {zone ? (
          <div className="ml-auto flex flex-wrap gap-2">
            <Button
              size="sm"
              variant="outline"
              onClick={() => setSheet({ kind: "add-mailbox" })}
            >
              Add mailbox
            </Button>
            <Button
              size="sm"
              variant="outline"
              onClick={() => setSheet({ kind: "add-forwarder" })}
            >
              Add forwarder
            </Button>
          </div>
        ) : null}
      </div>

      {/* Mailboxes ---------------------------------------------------------- */}
      {!mailboxes.loaded ? (
        <Skeleton className="h-16 w-full" />
      ) : mailboxes.error ? (
        <p role="alert" className="text-xs text-destructive">
          {mailboxes.error}
        </p>
      ) : mailboxes.items.length === 0 ? (
        <p className="text-ui rounded-[10px] border border-line px-5 py-6 text-muted-foreground">
          No mailboxes on this domain yet.
        </p>
      ) : (
        <div
          role="table"
          aria-label={`Mailboxes on ${domain.zoneName}`}
          className="overflow-hidden rounded-[10px] border border-line"
        >
          <div role="rowgroup">
            <div
              role="row"
              className={cn(
                "eyebrow hidden border-b border-line bg-fill-faint px-5 py-2.5 md:grid",
                mailboxHeaderColumns,
              )}
            >
              <div role="columnheader">Address</div>
              <div role="columnheader">Name</div>
              <div role="columnheader">Used</div>
              <div role="columnheader">Created</div>
              <div role="columnheader">
                <span className="sr-only">Actions</span>
              </div>
            </div>
          </div>
          <div role="rowgroup">
            {mailboxes.items.map((mailbox) => (
              <div
                key={mailbox.id}
                role="row"
                className={cn(
                  "text-ui grid grid-cols-1 gap-1.5 border-b border-line-soft px-5 py-4 last:border-0 hover:bg-row-hover md:items-center md:gap-0",
                  mailboxColumns,
                )}
              >
                <div role="cell" className="flex gap-2.5 md:block">
                  <span className="eyebrow w-16 shrink-0 pt-0.5 md:hidden">
                    Address
                  </span>
                  <span className="font-mono text-[13px] break-all">
                    {mailbox.address}
                  </span>
                </div>
                <div role="cell" className="flex gap-2.5 md:block">
                  <span className="eyebrow w-16 shrink-0 pt-0.5 md:hidden">
                    Name
                  </span>
                  <span className="text-subtle">
                    {mailbox.displayName || "—"}
                  </span>
                </div>
                <div role="cell" className="flex gap-2.5 md:block">
                  <span className="eyebrow w-16 shrink-0 pt-0.5 md:hidden">
                    Used
                  </span>
                  <Usage mailbox={mailbox} />
                </div>
                <div role="cell" className="flex gap-2.5 md:block">
                  <span className="eyebrow w-16 shrink-0 pt-0.5 md:hidden">
                    Created
                  </span>
                  <RelativeTime
                    date={toDate(mailbox.createdAt)}
                    className="font-mono text-[13px] text-muted-foreground"
                  />
                </div>
                <div role="cell" className="md:justify-self-end">
                  {zone ? (
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          aria-label={`Actions for ${mailbox.address}`}
                        >
                          <MoreVertical aria-hidden="true" className="size-4" />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem
                          onSelect={() => setSheet({ kind: "reset", mailbox })}
                        >
                          Reset password
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          onSelect={() =>
                            setSheet({ kind: "edit-mailbox", mailbox })
                          }
                        >
                          Change quota
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          variant="destructive"
                          onSelect={() =>
                            setSheet({ kind: "delete-mailbox", mailbox })
                          }
                        >
                          Delete
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  ) : null}
                </div>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Forwarders --------------------------------------------------------- */}
      {forwarders.loaded && forwarders.items.length > 0 ? (
        <div
          role="table"
          aria-label={`Forwarders on ${domain.zoneName}`}
          className="overflow-hidden rounded-[10px] border border-line"
        >
          <div role="rowgroup">
            <div
              role="row"
              className={cn(
                "eyebrow hidden border-b border-line bg-fill-faint px-5 py-2.5 md:grid",
                forwarderHeaderColumns,
              )}
            >
              <div role="columnheader">Address</div>
              <div role="columnheader">Targets</div>
              <div role="columnheader">Kind</div>
              <div role="columnheader" className="text-right">
                <span className="sr-only">Actions</span>
              </div>
            </div>
          </div>
          <div role="rowgroup">
            {forwarders.items.map((forwarder) => (
              <div
                key={forwarder.id}
                role="row"
                className={cn(
                  "text-ui grid grid-cols-1 gap-1.5 border-b border-line-soft px-5 py-4 last:border-0 hover:bg-row-hover md:items-center md:gap-0",
                  forwarderColumns,
                )}
              >
                <div role="cell" className="flex gap-2.5 md:block">
                  <span className="eyebrow w-16 shrink-0 pt-0.5 md:hidden">
                    Address
                  </span>
                  <span className="font-mono text-[13px] break-all">
                    {forwarder.address}
                  </span>
                </div>
                <div role="cell" className="flex gap-2.5 md:block">
                  <span className="eyebrow w-16 shrink-0 pt-0.5 md:hidden">
                    Targets
                  </span>
                  <span className="font-mono text-[13px] break-all text-subtle">
                    {forwarder.targets.join(", ")}
                  </span>
                </div>
                <div role="cell" className="flex gap-2.5 md:block">
                  <span className="eyebrow w-16 shrink-0 pt-0.5 md:hidden">
                    Kind
                  </span>
                  <span className="text-muted-foreground">
                    {forwarder.kind === ForwarderKind.ALIAS
                      ? "Alias"
                      : "Group forwarder"}
                    {forwarder.external ? " · external" : ""}
                  </span>
                </div>
                <div role="cell" className="md:text-right">
                  {zone ? (
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() =>
                        setSheet({ kind: "delete-forwarder", forwarder })
                      }
                    >
                      Delete
                    </Button>
                  ) : null}
                </div>
              </div>
            ))}
          </div>
        </div>
      ) : null}

      {/* Records ------------------------------------------------------------ */}
      {domain.records.length ? (
        <div className="flex flex-col gap-2 rounded-[10px] border border-line px-5 py-4">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <p className="eyebrow">Automatic records</p>
            {zone && !domain.zoneMissing ? (
              <Button
                variant="outline"
                size="sm"
                disabled={reapplying}
                onClick={() => {
                  void reapply();
                }}
              >
                {reapplying ? "Re-applying…" : "Re-apply"}
              </Button>
            ) : null}
          </div>
          <ul className="flex flex-col gap-1.5 font-mono text-[13px]">
            {domain.records.map((record) => (
              <li
                key={`${record.type}-${record.name}-${record.value}`}
                className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5"
              >
                <StatusDot
                  tone={record.present && record.matches ? "success" : "warning"}
                  label={
                    record.present && record.matches
                      ? "published"
                      : record.present
                        ? "different value in the zone"
                        : "missing from the zone"
                  }
                />
                <span className="text-link">{record.type}</span>
                <span>{record.name}</span>
                <span className="text-soft break-all">{record.value}</span>
                {!record.present ? (
                  <span className="font-sans text-xs text-warning">missing</span>
                ) : !record.matches ? (
                  <span className="font-sans text-xs text-warning">
                    different value in the zone
                  </span>
                ) : null}
              </li>
            ))}
          </ul>
          {domain.dkimPending ? (
            <p className="text-xs text-warning">
              DKIM keys are still being generated — the records are completed
              automatically within a minute.
            </p>
          ) : null}
          {reapplyError ? (
            <p role="alert" className="text-xs text-destructive">
              {reapplyError}
            </p>
          ) : null}

          {zone && !domain.zoneMissing ? (
            <div className="mt-1 flex flex-col gap-1.5 border-t border-line-soft pt-3">
              <div className="flex items-start gap-2">
                <Checkbox
                  id={`autoconfig-${domain.zoneId}`}
                  checked={domain.publishClientAutoconfig}
                  disabled={autoconfigBusy}
                  onCheckedChange={(checked) => {
                    void setClientAutoconfig(checked === true);
                  }}
                  className="mt-0.5"
                />
                <Label
                  htmlFor={`autoconfig-${domain.zoneId}`}
                  className="flex flex-col items-start gap-0.5 leading-normal font-normal"
                >
                  Publish mail client setup records
                  <span className="text-xs text-muted-foreground">
                    The SRV records and autoconfig/autodiscover aliases that let
                    Thunderbird, Outlook and Apple Mail configure themselves.
                    {autoconfigBusy ? " Saving…" : ""}
                  </span>
                </Label>
              </div>
              {autoconfigError ? (
                <p role="alert" className="text-xs text-destructive">
                  {autoconfigError}
                </p>
              ) : null}
            </div>
          ) : null}
        </div>
      ) : null}

      {/* Dialogs ------------------------------------------------------------ */}
      {zone && sheet?.kind === "add-mailbox" ? (
        <MailboxDialog
          zone={zone}
          mode="create"
          limit={limit}
          count={mailboxes.items.length}
          open
          onOpenChange={(next) => {
            if (!next) {
              setSheet(undefined);
            }
          }}
          onDone={(result) => {
            // `onDone` owns the transition out of the create dialog: the
            // one-time password lives only in this result and in the screen it
            // is handed to.
            if (result.$typeName === "mail.v1.CreateMailboxResponse") {
              setSheet({
                kind: "created",
                address: result.mailbox?.address ?? "",
                password: result.password,
                retrievalProtocol: result.retrievalProtocol,
                retrievalHost: result.retrievalHost,
                retrievalPort: result.retrievalPort,
                smtpHost: result.smtpHost,
                smtpPort: result.smtpPort,
              });
            } else {
              setSheet(undefined);
            }
            void mailboxes.refresh();
          }}
        />
      ) : null}

      {zone && sheet?.kind === "edit-mailbox" ? (
        <MailboxDialog
          zone={zone}
          mode="edit"
          mailbox={sheet.mailbox}
          limit={limit}
          count={mailboxes.items.length}
          open
          onOpenChange={(next) => {
            if (!next) {
              setSheet(undefined);
            }
          }}
          onDone={() => {
            setSheet(undefined);
            void mailboxes.refresh();
          }}
        />
      ) : null}

      {sheet?.kind === "created" ? (
        <MailboxCreatedDialog
          address={sheet.address}
          password={sheet.password}
          retrievalProtocol={sheet.retrievalProtocol}
          retrievalHost={sheet.retrievalHost}
          retrievalPort={sheet.retrievalPort}
          smtpHost={sheet.smtpHost}
          smtpPort={sheet.smtpPort}
          open
          onOpenChange={(next) => {
            if (!next) {
              setSheet(undefined);
            }
          }}
        />
      ) : null}

      {zone && sheet?.kind === "reset" ? (
        <ResetPasswordDialog
          zone={zone}
          mailbox={sheet.mailbox}
          open
          onOpenChange={(next) => {
            if (!next) {
              setSheet(undefined);
            }
          }}
        />
      ) : null}

      {zone && sheet?.kind === "delete-mailbox" ? (
        <DeleteMailboxDialog
          zone={zone}
          mailbox={sheet.mailbox}
          open
          onOpenChange={(next) => {
            if (!next) {
              setSheet(undefined);
            }
          }}
          onDeleted={() => {
            void mailboxes.refresh();
          }}
        />
      ) : null}

      {zone && sheet?.kind === "add-forwarder" ? (
        <ForwarderDialog
          zone={zone}
          mailboxes={mailboxes.items}
          open
          onOpenChange={(next) => {
            if (!next) {
              setSheet(undefined);
            }
          }}
          onDone={() => {
            void forwarders.refresh();
            void mailboxes.refresh();
          }}
        />
      ) : null}

      {zone && sheet?.kind === "delete-forwarder" ? (
        <DeleteForwarderDialog
          zone={zone}
          forwarder={sheet.forwarder}
          open
          onOpenChange={(next) => {
            if (!next) {
              setSheet(undefined);
            }
          }}
          onDeleted={() => {
            void forwarders.refresh();
            void mailboxes.refresh();
          }}
        />
      ) : null}
    </section>
  );
}

/**
 * `quota_bytes === 0` means the mail server enforces no quota, so there is no bar to
 * draw and no percentage to claim — only what is used.
 */
function Usage({ mailbox }: { mailbox: Mailbox }) {
  const used = Number(mailbox.usedBytes);
  const quota = Number(mailbox.quotaBytes);
  if (quota <= 0) {
    return (
      <span className="text-subtle">{formatBytes(used)} · no quota</span>
    );
  }
  const percent = Math.min(100, Math.round((used / quota) * 100));
  return (
    <span className="flex min-w-0 flex-col gap-1">
      <span className={cn("text-subtle", percent >= 90 && "text-warning")}>
        {formatBytes(used)} of {formatBytes(quota)}
      </span>
      <Progress
        value={percent}
        aria-label={`${percent}% of the mailbox used`}
        className="bg-fill"
        indicatorClassName={percent >= 90 ? "bg-warning" : "bg-primary"}
      />
    </span>
  );
}
