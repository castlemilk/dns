"use client";

import { useCallback, useState } from "react";

import { usePlatform } from "@/components/app/platform-provider";
import { RecordChangeList } from "@/components/app/record-change-list";
import { useRecordPreview } from "@/components/hosting/use-record-preview";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import type {
  BindMailDomainResponse,
  GetMailStatusResponse,
} from "@/gen/mail/v1/mail_pb";
import type { RecordChange } from "@/gen/platform/v1/platform_pb";
import { stripDot } from "@/lib/dns-values";
import { describeError } from "@/lib/errors";
import { plural } from "@/lib/format";
import { getMailClient } from "@/lib/platform-client";

export type BindMailDialogProps = {
  zone: Zone;
  mailStatus: GetMailStatusResponse;
  open: boolean;
  onOpenChange(open: boolean): void;
  onDone?(result: BindMailDomainResponse): void;
};

const policies = [
  { value: "none", label: "none — monitor only" },
  { value: "quarantine", label: "quarantine — send failures to spam" },
  { value: "reject", label: "reject — refuse failures" },
] as const;

/** The report address is only known from the plan the control plane returned. */
function reportAddressOf(changes: RecordChange[]): string {
  const dmarc = changes.find((change) => change.name === "_dmarc");
  const match = /rua=mailto:([^;"\s]+)/.exec(dmarc?.value ?? "");
  return match ? match[1] : "";
}

/** The mail host an existing MX conflict routes to, for the "stops delivery" warning. */
function currentMailHost(conflicts: RecordChange[]): string {
  const mx = conflicts.find((conflict) => conflict.type === "MX");
  if (!mx) {
    return "";
  }
  const parts = mx.value.trim().split(/\s+/);
  return stripDot(parts[parts.length - 1] ?? "");
}

/**
 * Binding a domain to the mail server writes MX, SPF, DKIM and DMARC into this zone.
 * The plan is fetched with `dry_run: true` first, so the exact records — including the
 * DKIM selectors the mail server will generate — are on screen before anything moves.
 */
export function BindMailDialog({
  zone,
  mailStatus,
  open,
  onOpenChange,
  onDone,
}: BindMailDialogProps) {
  const { invalidate } = usePlatform();
  const [policy, setPolicy] = useState("quarantine");
  // Absent means on engine-side; the dialog is explicit either way so the dry-run plan
  // on screen is exactly the set the bind will write.
  const [autoconfig, setAutoconfig] = useState(true);
  const [replace, setReplace] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [pendingNote, setPendingNote] = useState<string>();

  const runPreview = useCallback(
    async (signal: AbortSignal) => {
      const response = await getMailClient().bindMailDomain(
        {
          zoneId: zone.id,
          dmarcPolicy: policy,
          publishClientAutoconfig: autoconfig,
          dryRun: true,
        },
        { signal },
      );
      return { dnsPlan: response.dnsPlan, conflicts: response.conflicts };
    },
    [autoconfig, policy, zone.id],
  );

  const preview = useRecordPreview(open, `${policy}:${autoconfig}`, runPreview);
  const conflicts = preview.conflicts.length;
  const reportAddress = reportAddressOf(preview.changes);
  const replacedHost = currentMailHost(preview.conflicts);

  async function handleSubmit() {
    setError(undefined);
    setBusy(true);
    try {
      const result = await getMailClient().bindMailDomain({
        zoneId: zone.id,
        dmarcPolicy: policy,
        publishClientAutoconfig: autoconfig,
        replaceConflictingRecords: replace,
        dryRun: false,
      });
      await invalidate();
      onDone?.(result);
      if (result.domain?.dkimPending) {
        setPendingNote(
          "DKIM keys are still being generated — the records are completed automatically within a minute.",
        );
        setBusy(false);
        return;
      }
      onOpenChange(false);
    } catch (caught) {
      setError(describeError(caught));
      setBusy(false);
    }
  }

  if (!open) {
    return null;
  }

  return (
    <Dialog
      open
      onOpenChange={(next) => {
        if (!next && busy) {
          return;
        }
        onOpenChange(next);
      }}
    >
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Host mail for {zone.name}</DialogTitle>
          <DialogDescription>
            MX, SPF, DKIM and DMARC records for{" "}
            <span className="font-mono">{mailStatus.mailHostname}</span> are
            written into this zone.
            {reportAddress ? (
              <> Reports go to {reportAddress}.</>
            ) : null}
          </DialogDescription>
        </DialogHeader>

        {pendingNote ? (
          <p className="text-ui text-warning">{pendingNote}</p>
        ) : (
          <div className="flex flex-col gap-3">
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="bind-dmarc">DMARC policy</Label>
              <Select value={policy} onValueChange={setPolicy} disabled={busy}>
                <SelectTrigger id="bind-dmarc" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {policies.map((option) => (
                    <SelectItem key={option.value} value={option.value}>
                      {option.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            <div className="flex items-start gap-2">
              <Checkbox
                id="bind-autoconfig"
                checked={autoconfig}
                onCheckedChange={(checked) => setAutoconfig(checked === true)}
                disabled={busy}
                className="mt-0.5"
              />
              <Label
                htmlFor="bind-autoconfig"
                className="flex flex-col items-start gap-0.5 leading-normal font-normal"
              >
                Publish mail client setup records
                <span className="text-xs text-muted-foreground">
                  SRV records and autoconfig/autodiscover aliases, so Thunderbird,
                  Outlook and Apple Mail find the server without being told.
                </span>
              </Label>
            </div>

            <div className="flex flex-col gap-2 rounded-[10px] border border-line bg-fill-faint px-3.5 py-3">
              <p className="eyebrow">Records to write</p>
              {preview.loading && !preview.loaded ? (
                <div className="flex flex-col gap-1.5">
                  <Skeleton className="h-4 w-3/4" />
                  <Skeleton className="h-4 w-2/3" />
                </div>
              ) : preview.error ? (
                <p role="alert" className="text-xs text-destructive">
                  {preview.error}
                </p>
              ) : (
                <RecordChangeList
                  changes={preview.changes}
                  conflicts={preview.conflicts}
                />
              )}
            </div>

            {replacedHost ? (
              <p className="text-ui text-warning">
                {zone.name} currently routes mail to {replacedHost} — binding it
                here replaces those records and stops delivery there.
              </p>
            ) : null}

            {conflicts > 0 ? (
              <div className="flex items-start gap-2">
                <Checkbox
                  id="bind-replace"
                  checked={replace}
                  onCheckedChange={(checked) => setReplace(checked === true)}
                  disabled={busy}
                  className="mt-0.5"
                />
                <Label
                  htmlFor="bind-replace"
                  className="font-normal text-warning"
                >
                  Replace the {conflicts} custom {plural(conflicts, "record")}{" "}
                  listed above
                </Label>
              </div>
            ) : null}

            {error ? (
              <p role="alert" className="text-xs text-destructive">
                {error}
              </p>
            ) : null}
          </div>
        )}

        <DialogFooter>
          {pendingNote ? (
            <Button onClick={() => onOpenChange(false)}>Close</Button>
          ) : (
            <>
              <Button
                variant="outline"
                onClick={() => onOpenChange(false)}
                disabled={busy}
              >
                Cancel
              </Button>
              <Button
                onClick={() => {
                  void handleSubmit();
                }}
                disabled={busy || (conflicts > 0 && !replace)}
              >
                {busy ? "Binding…" : "Bind and write records"}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
