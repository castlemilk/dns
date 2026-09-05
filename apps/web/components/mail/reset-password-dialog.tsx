"use client";

import { useState } from "react";

import { CopyButton } from "@/components/app/copy-button";
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
import type {
  Mailbox,
  ResetMailboxPasswordResponse,
} from "@/gen/mail/v1/mail_pb";
import { describeError } from "@/lib/errors";
import { getMailClient } from "@/lib/platform-client";

export type ResetPasswordDialogProps = {
  zone: Zone;
  mailbox: Mailbox;
  open: boolean;
  onOpenChange(open: boolean): void;
  onDone?(result: ResetMailboxPasswordResponse): void;
};

/**
 * Resets one mailbox password. The new password is shown here once and nowhere else —
 * the control plane never stores it and never records it in the event log.
 */
export function ResetPasswordDialog({
  zone,
  mailbox,
  open,
  onOpenChange,
  onDone,
}: ResetPasswordDialogProps) {
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function handleReset() {
    setError(undefined);
    setBusy(true);
    try {
      const result = await getMailClient().resetMailboxPassword({
        zoneId: zone.id,
        mailboxId: mailbox.id,
      });
      setPassword(result.password);
      setBusy(false);
      onDone?.(result);
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
      <DialogContent
        showCloseButton={password === ""}
        onInteractOutside={(event) => {
          if (password !== "") {
            event.preventDefault();
          }
        }}
        // Once the password is on screen this is the same one-time-secret dialog as
        // MailboxCreatedDialog and it guards the same way: Escape would throw away a
        // value nothing can show again. Before the reset, Escape closes normally.
        onEscapeKeyDown={(event) => {
          if (password !== "") {
            event.preventDefault();
          }
        }}
        className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-md"
      >
        <DialogHeader>
          <DialogTitle>Reset the password for {mailbox.address}?</DialogTitle>
          <DialogDescription>
            {password
              ? "This is the only time the new password is shown. The control plane does not keep a copy, so Escape and clicking outside are ignored here — copy it, then close with “I’ve saved it”."
              : "Existing sessions and app passwords are signed out. The new password is shown once and is not stored here."}
          </DialogDescription>
        </DialogHeader>

        {password ? (
          <div className="flex flex-col gap-2">
            <p className="eyebrow">New password</p>
            <code
              role="status"
              aria-live="polite"
              className="rounded-[10px] border border-line bg-fill-faint px-3.5 py-3 font-mono text-[15px] break-all select-all"
            >
              {password}
            </code>
            <CopyButton
              value={password}
              valueLabel="password"
              className="self-start text-[13px]"
            />
          </div>
        ) : null}

        {error ? (
          <p role="alert" className="text-xs text-destructive">
            {error}
          </p>
        ) : null}

        <DialogFooter>
          {password ? (
            <Button onClick={() => onOpenChange(false)}>
              I&apos;ve saved it
            </Button>
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
                variant="destructive"
                onClick={() => {
                  void handleReset();
                }}
                disabled={busy}
              >
                {busy ? "Resetting…" : "Reset password"}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
