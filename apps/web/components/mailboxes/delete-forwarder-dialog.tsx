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
import type { Zone } from "@/gen/dns/v1/dns_pb";
import { ForwarderKind, type Forwarder } from "@/gen/mail/v1/mail_pb";
import { describeError } from "@/lib/errors";
import { getMailClient } from "@/lib/platform-client";

export type DeleteForwarderDialogProps = {
  zone: Zone;
  forwarder: Forwarder;
  open: boolean;
  onOpenChange(open: boolean): void;
  onDeleted?(): void;
};

/** Removes one forwarding address; the mailboxes it pointed at are untouched. */
export function DeleteForwarderDialog({
  zone,
  forwarder,
  open,
  onOpenChange,
  onDeleted,
}: DeleteForwarderDialogProps) {
  const { invalidate } = usePlatform();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function handleDelete() {
    setError(undefined);
    setBusy(true);
    try {
      await getMailClient().deleteForwarder({
        zoneId: zone.id,
        forwarderId: forwarder.id,
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
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Delete {forwarder.address}?</DialogTitle>
          <DialogDescription>
            {forwarder.kind === ForwarderKind.ALIAS
              ? "The alias is removed from the mailbox it delivers into; the mailbox itself is kept."
              : "The group forwarder is removed from the mail server; the addresses it forwarded to are untouched."}
          </DialogDescription>
        </DialogHeader>

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
            disabled={busy}
          >
            {busy ? "Deleting…" : "Delete forwarder"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
