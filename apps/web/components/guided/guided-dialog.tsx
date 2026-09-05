"use client";

import { useState } from "react";

import { DismissGuardNote } from "@/components/app/dismiss-guard-note";
import { PointWebsiteForm } from "@/components/guided/point-website-form";
import { RouteEmailForm } from "@/components/guided/route-email-form";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import type { Zone } from "@/gen/dns/v1/dns_pb";

export type GuidedDialogProps = {
  kind: "website" | "email";
  open: boolean;
  zone: Zone;
  onOpenChange: (open: boolean) => void;
  onApplied?: (zone: Zone) => void;
};

/**
 * The dialog wrapper around the two guided forms. Mounted conditionally by its owner so
 * the form re-seeds from the current zone every time it opens.
 */
export function GuidedDialog({
  kind,
  open,
  zone,
  onOpenChange,
  onApplied,
}: GuidedDialogProps) {
  // While a plan is being written, Escape and a click on the overlay are ignored:
  // unmounting the form mid-apply would drop the progress line and, if a later record
  // fails, the "Wrote 1 of 3 records, then failed…" report with it.
  const [writing, setWriting] = useState(false);

  function handleApplied(written: Zone) {
    setWriting(false);
    onOpenChange(false);
    onApplied?.(written);
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next && writing) {
          return;
        }
        onOpenChange(next);
      }}
    >
      <DialogContent
        className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-xl"
        showCloseButton={!writing}
        onEscapeKeyDown={(event) => {
          if (writing) {
            event.preventDefault();
          }
        }}
        onInteractOutside={(event) => {
          if (writing) {
            event.preventDefault();
          }
        }}
      >
        <DialogHeader>
          <DialogTitle>
            {kind === "website" ? "Point website here" : "Route email"}
          </DialogTitle>
          <DialogDescription>
            Records are written to{" "}
            <span className="font-mono text-foreground">{zone.name}</span>{" "}
            through the API; you&rsquo;ll see them in the raw zone.
          </DialogDescription>
        </DialogHeader>

        <DismissGuardNote active={writing} action="Writing the records" />

        {kind === "website" ? (
          <PointWebsiteForm
            zone={zone}
            layout="dialog"
            submitLabel="Write records"
            onApplied={handleApplied}
            onCancel={() => onOpenChange(false)}
            onSubmittingChange={setWriting}
          />
        ) : (
          <RouteEmailForm
            zone={zone}
            layout="dialog"
            submitLabel="Write records"
            onApplied={handleApplied}
            onCancel={() => onOpenChange(false)}
            onSubmittingChange={setWriting}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}
