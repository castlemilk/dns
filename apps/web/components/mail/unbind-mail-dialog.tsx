"use client";

import { useState } from "react";

import { usePlatform } from "@/components/app/platform-provider";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import type {
  Forwarder,
  MailDomain,
  Mailbox,
  UnbindMailDomainResponse,
} from "@/gen/mail/v1/mail_pb";
import { lower } from "@/lib/dns-values";
import { describeError } from "@/lib/errors";
import { plural } from "@/lib/format";
import { getMailClient } from "@/lib/platform-client";

export type UnbindMailDialogProps = {
  zone: Zone;
  mailDomain: MailDomain;
  /** Listed in the dialog: unbinding deletes them on the mail server. */
  mailboxes: Mailbox[];
  forwarders: Forwarder[];
  open: boolean;
  onOpenChange(open: boolean): void;
  onDone?(result: UnbindMailDomainResponse): void;
};

/**
 * Unbinding destroys the mailboxes and forwarders on the mail server, so everything
 * that will be deleted is listed and the domain name has to be typed first.
 */
export function UnbindMailDialog({
  zone,
  mailDomain,
  mailboxes,
  forwarders,
  open,
  onOpenChange,
  onDone,
}: UnbindMailDialogProps) {
  const { invalidate } = usePlatform();
  const [records, setRecords] = useState<"keep" | "remove">("remove");
  const [confirmation, setConfirmation] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const hasContents = mailboxes.length > 0 || forwarders.length > 0;
  const confirmed = !hasContents || lower(confirmation) === lower(zone.name);

  async function handleSubmit() {
    setError(undefined);
    setBusy(true);
    try {
      const result = await getMailClient().unbindMailDomain({
        zoneId: zone.id,
        deleteMailboxes: hasContents,
        keepDnsRecords: records === "keep",
        confirmZoneName: hasContents ? zone.name : "",
      });
      await invalidate();
      onDone?.(result);
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
          <DialogTitle>Unbind mail from {zone.name}?</DialogTitle>
          <DialogDescription>
            {hasContents
              ? `${mailboxes.length} ${plural(mailboxes.length, "mailbox", "mailboxes")} and ${forwarders.length} ${plural(forwarders.length, "forwarder")} are deleted on the mail server, with everything they hold.`
              : "This domain has no mailboxes or forwarders."}
          </DialogDescription>
        </DialogHeader>

        {hasContents ? (
          <ul className="flex max-h-40 flex-col gap-1 overflow-y-auto rounded-[10px] border border-line bg-fill-faint px-3.5 py-3 font-mono text-[13px] text-subtle">
            {mailboxes.map((mailbox) => (
              <li key={mailbox.id} className="break-all">
                {mailbox.address}
              </li>
            ))}
            {forwarders.map((forwarder) => (
              <li key={forwarder.id} className="break-all">
                {forwarder.address} → {forwarder.targets.join(", ")}
              </li>
            ))}
          </ul>
        ) : null}

        <fieldset className="flex flex-col gap-2">
          <legend className="text-sm font-medium">
            The {mailDomain.records.length} automatic DNS records
          </legend>
          <RadioGroup
            value={records}
            onValueChange={(value) =>
              setRecords(value === "keep" ? "keep" : "remove")
            }
          >
            <div className="flex items-start gap-2">
              <RadioGroupItem
                id="unbind-remove"
                value="remove"
                disabled={busy}
                className="mt-1"
              />
              <Label htmlFor="unbind-remove" className="font-normal">
                Remove them
              </Label>
            </div>
            <div className="flex items-start gap-2">
              <RadioGroupItem
                id="unbind-keep"
                value="keep"
                disabled={busy}
                className="mt-1"
              />
              <Label htmlFor="unbind-keep" className="font-normal">
                Keep the DNS records as custom records
              </Label>
            </div>
          </RadioGroup>
        </fieldset>

        {hasContents ? (
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="unbind-confirm">Type {zone.name} to confirm</Label>
            <Input
              id="unbind-confirm"
              value={confirmation}
              onChange={(event) => setConfirmation(event.target.value)}
              placeholder={zone.name}
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              disabled={busy}
              className="font-mono"
            />
          </div>
        ) : null}

        {error ? (
          <p role="alert" className="text-xs text-destructive">
            {error}
          </p>
        ) : null}

        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={busy}
          >
            Cancel
          </Button>
          <Button
            variant="destructive"
            onClick={() => {
              void handleSubmit();
            }}
            disabled={busy || !confirmed}
          >
            {busy ? "Unbinding…" : "Unbind mail"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
