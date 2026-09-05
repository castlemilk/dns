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
import { Textarea } from "@/components/ui/textarea";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import type { CreateForwarderResponse, Mailbox } from "@/gen/mail/v1/mail_pb";
import { lower } from "@/lib/dns-values";
import { describeError } from "@/lib/errors";
import { plural } from "@/lib/format";
import { getMailClient } from "@/lib/platform-client";

export type ForwarderDialogProps = {
  zone: Zone;
  /** Mailboxes on this domain: a target that matches one makes the forwarder an alias. */
  mailboxes: Mailbox[];
  open: boolean;
  onOpenChange(open: boolean): void;
  onDone?(result: CreateForwarderResponse): void;
};

const localPartPattern = /^[a-z0-9](?:[a-z0-9._+-]{0,62}[a-z0-9])?$/;
const addressPattern = /^[^\s@]+@[^\s@.]+(\.[^\s@.]+)+$/;

export const srsNote =
  "Forwarded mail keeps the original sender, so some providers may treat it as spam. This mail server adds ARC headers but does not rewrite the sender (no SRS).";

/**
 * Creates one forwarding address. Exactly one target that is a mailbox on this domain
 * becomes an alias on that mailbox; anything else becomes a group forwarder.
 */
export function ForwarderDialog({
  zone,
  mailboxes,
  open,
  onOpenChange,
  onDone,
}: ForwarderDialogProps) {
  const { invalidate } = usePlatform();
  const [localPart, setLocalPart] = useState("");
  const [targetText, setTargetText] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const normalised = lower(localPart);
  const localInvalid =
    normalised.length > 0 &&
    (!localPartPattern.test(normalised) || normalised.includes(".."));

  const targets = targetText
    .split("\n")
    .map((line) => lower(line))
    .filter((line) => line.length > 0);
  const invalidTargets = targets.filter(
    (target) => !addressPattern.test(target),
  );

  const localMailbox =
    targets.length === 1
      ? mailboxes.find((mailbox) => lower(mailbox.address) === targets[0])
      : undefined;
  const external = targets.filter(
    (target) => !target.endsWith(`@${lower(zone.name)}`),
  );

  async function handleSubmit() {
    setError(undefined);
    setBusy(true);
    try {
      const result = await getMailClient().createForwarder({
        zoneId: zone.id,
        localPart: normalised,
        targets,
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
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Add a forwarder on {zone.name}</DialogTitle>
          <DialogDescription>
            Mail sent to this address is delivered to the targets below.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-3">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="forwarder-local">Address</Label>
            <div className="flex items-center gap-1.5">
              <Input
                id="forwarder-local"
                value={localPart}
                onChange={(event) => setLocalPart(event.target.value)}
                placeholder="hello"
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                disabled={busy}
                aria-invalid={localInvalid || undefined}
                className="font-mono text-[13px]"
              />
              <span className="font-mono text-[13px] text-muted-foreground">
                @{zone.name}
              </span>
            </div>
            {localInvalid ? (
              <p role="alert" className="text-xs text-destructive">
                Use letters, digits and . _ + - — starting and ending with a
                letter or digit.
              </p>
            ) : null}
          </div>

          <div className="flex flex-col gap-1.5">
            <Label htmlFor="forwarder-targets">Targets, one per line</Label>
            <Textarea
              id="forwarder-targets"
              mono
              value={targetText}
              onChange={(event) => setTargetText(event.target.value)}
              placeholder={`mara@${zone.name}`}
              autoCapitalize="none"
              spellCheck={false}
              disabled={busy}
              aria-invalid={invalidTargets.length > 0 || undefined}
            />
            {invalidTargets.length ? (
              <p role="alert" className="text-xs text-destructive">
                Not an email address: {invalidTargets.join(", ")}
              </p>
            ) : null}
          </div>

          {targets.length ? (
            <p className="text-ui text-subtle">
              {localMailbox
                ? `Delivers into ${localMailbox.localPart}'s mailbox`
                : external.length
                  ? `Forwards to ${external.length} external ${plural(
                      external.length,
                      "address",
                      "addresses",
                    )}`
                  : `Forwards to ${targets.length} ${plural(
                      targets.length,
                      "address",
                      "addresses",
                    )} on this domain`}
            </p>
          ) : null}

          {external.length ? (
            <p className="text-xs text-muted-foreground">{srsNote}</p>
          ) : null}

          {error ? (
            <p role="alert" className="text-xs text-destructive">
              {error}
            </p>
          ) : null}
        </div>

        <DialogFooter>
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
            disabled={
              busy ||
              localInvalid ||
              normalised.length === 0 ||
              targets.length === 0 ||
              targets.length > 10 ||
              invalidTargets.length > 0
            }
          >
            {busy ? "Adding…" : "Add forwarder"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
