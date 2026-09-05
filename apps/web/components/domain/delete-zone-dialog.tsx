"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";

import { useZones } from "@/components/app/zones-provider";
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
import { Input } from "@/components/ui/input";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import { lower } from "@/lib/dns-values";
import { describeError } from "@/lib/errors";
import { plural } from "@/lib/format";

export type DeleteZoneDialogProps = {
  zone: Zone;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onDeleted: () => void;
  /**
   * Fired with `true` as the delete starts and `false` if it fails. The zone
   * leaves the store as soon as the RPC returns, before `router.replace` commits,
   * so the page uses this to hold its loading state instead of flashing
   * "no domain called X here" for the length of that navigation.
   */
  onDeletingChange?: (deleting: boolean) => void;
};

export function DeleteZoneDialog({
  zone,
  open,
  onOpenChange,
  onDeleted,
  onDeletingChange,
}: DeleteZoneDialogProps) {
  const { deleteZone } = useZones();
  const router = useRouter();
  const [confirmation, setConfirmation] = useState("");
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState<string>();

  const count = zone.records.length;
  const confirmed = lower(confirmation) === lower(zone.name);

  async function handleConfirm() {
    setError(undefined);
    setDeleting(true);
    onDeletingChange?.(true);
    try {
      await deleteZone(zone.id);
      router.replace("/domains");
      onDeleted();
    } catch (caught) {
      setError(describeError(caught));
      setDeleting(false);
      onDeletingChange?.(false);
    }
  }

  return (
    <AlertDialog
      open={open}
      onOpenChange={(next) => {
        if (!next && deleting) {
          return;
        }
        onOpenChange(next);
      }}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Delete {zone.name}?</AlertDialogTitle>
          <AlertDialogDescription>
            All {count} {plural(count, "record")} stop resolving from this
            service. Type the domain to confirm.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <Input
          aria-label="Domain name to confirm"
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          placeholder={zone.name}
          value={confirmation}
          onChange={(event) => setConfirmation(event.target.value)}
          disabled={deleting}
          className="font-mono"
        />
        {error ? (
          <p role="alert" className="text-xs text-destructive">
            {error}
          </p>
        ) : null}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={deleting}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={deleting || !confirmed}
            onClick={(event) => {
              event.preventDefault();
              void handleConfirm();
            }}
          >
            {deleting ? "Deleting…" : "Delete domain"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
