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
import type { Zone } from "@/gen/dns/v1/dns_pb";
import type { Mailbox } from "@/gen/mail/v1/mail_pb";
import { lower } from "@/lib/dns-values";
import { describeError } from "@/lib/errors";
import { getMailClient } from "@/lib/platform-client";

export type DeleteMailboxDialogProps = {
  zone: Zone;
  mailbox: Mailbox;
  open: boolean;
  onOpenChange(open: boolean): void;
  onDeleted?(): void;
};

/**
 * The mail server deletes the account and everything in it. The control plane requires
 * the address back, so the aliases that go with it are visible before it happens.
 */
export function DeleteMailboxDialog({
  zone,
  mailbox,
  open,
  onOpenChange,
  onDeleted,
}: DeleteMailboxDialogProps) {
  const { invalidate } = usePlatform();
  const [confirmation, setConfirmation] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const confirmed = lower(confirmation) === lower(mailbox.address);

  async function handleDelete() {
    setError(undefined);
    setBusy(true);
    try {
      await getMailClient().deleteMailbox({
        zoneId: zone.id,
        mailboxId: mailbox.id,
        confirmAddress: mailbox.address,
      });
      await invalidate();
      onDeleted?.();
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
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Delete {mailbox.address}?</DialogTitle>
          <DialogDescription>
            The mailbox and everything stored in it are removed from the mail
            server. Type the address to confirm.
          </DialogDescription>
        </DialogHeader>

        {mailbox.aliases.length ? (
          <p className="text-ui text-warning">
            These addresses stop working too:{" "}
            <span className="font-mono text-[13px] break-all">
              {mailbox.aliases
                .map((alias) => `${alias}@${zone.name}`)
                .join(", ")}
            </span>
          </p>
        ) : null}

        <Input
          aria-label="Mailbox address to confirm"
          value={confirmation}
          onChange={(event) => setConfirmation(event.target.value)}
          placeholder={mailbox.address}
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          disabled={busy}
          className="font-mono"
        />
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
              void handleDelete();
            }}
            disabled={busy || !confirmed}
          >
            {busy ? "Deleting…" : "Delete mailbox"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
