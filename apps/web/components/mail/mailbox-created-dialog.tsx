"use client";

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

export type MailboxCreatedDialogProps = {
  address: string;
  /**
   * The one-time password from `CreateMailbox`/`ResetMailboxPassword`. It exists only in
   * this component's props: it is never logged, stored or sent anywhere else.
   */
  password: string;
  imapHost: string;
  imapPort: number;
  smtpHost: string;
  smtpPort: number;
  open: boolean;
  onOpenChange(open: boolean): void;
};

/**
 * The only place the generated password is ever rendered. The control plane does not
 * keep it, so the dialog cannot be dismissed by clicking outside — the person has to
 * confirm they saved it.
 */
export function MailboxCreatedDialog({
  address,
  password,
  imapHost,
  imapPort,
  smtpHost,
  smtpPort,
  open,
  onOpenChange,
}: MailboxCreatedDialogProps) {
  if (!open) {
    return null;
  }

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent
        showCloseButton={false}
        onInteractOutside={(event) => event.preventDefault()}
        onEscapeKeyDown={(event) => event.preventDefault()}
        className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-md"
      >
        <DialogHeader>
          <DialogTitle>{address} is ready</DialogTitle>
          {/* The dialog deliberately ignores Escape and a click outside, so it has to
              say so: a keyboard user who presses Escape and sees nothing happen
              otherwise has no way to tell a guard from a broken dialog. The guard is
              named in the description, which Radix wires to the dialog's
              aria-describedby, so it is read out on open. */}
          <DialogDescription>
            This is the only time it is shown. The control plane does not keep a
            copy, so Escape and clicking outside are ignored here — copy the
            password, then close with &ldquo;I&rsquo;ve saved it&rdquo;.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-2">
          <p className="eyebrow">Password</p>
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

        <dl className="text-ui grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
          <dt className="text-muted-foreground">IMAP</dt>
          <dd className="font-mono text-[13px]">
            {imapHost}:{imapPort}
          </dd>
          <dt className="text-muted-foreground">SMTP</dt>
          <dd className="font-mono text-[13px]">
            {smtpHost}:{smtpPort}
          </dd>
          <dt className="text-muted-foreground">Username</dt>
          <dd className="font-mono text-[13px] break-all">{address}</dd>
        </dl>

        <DialogFooter>
          <Button onClick={() => onOpenChange(false)}>I&apos;ve saved it</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
