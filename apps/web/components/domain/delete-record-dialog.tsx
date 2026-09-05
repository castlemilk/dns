"use client";

import { useState } from "react";

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
import type { Record as DNSRecord } from "@/gen/dns/v1/dns_pb";
import { fqdn, recordTypeName } from "@/lib/dns-values";
import { describeError } from "@/lib/errors";

export type DeleteRecordDialogProps = {
  record?: DNSRecord;
  zoneName: string;
  onConfirm: () => Promise<void>;
  onOpenChange: (open: boolean) => void;
};

export function DeleteRecordDialog({
  record,
  zoneName,
  onConfirm,
  onOpenChange,
}: DeleteRecordDialogProps) {
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState<string>();

  async function handleConfirm() {
    setError(undefined);
    setDeleting(true);
    try {
      await onConfirm();
    } catch (caught) {
      // The dialog stays open so the message sits next to the action that failed.
      setError(describeError(caught));
    } finally {
      setDeleting(false);
    }
  }

  return (
    <AlertDialog
      open={Boolean(record)}
      onOpenChange={(open) => {
        if (!open && deleting) {
          return;
        }
        onOpenChange(open);
      }}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Delete record?</AlertDialogTitle>
          <AlertDialogDescription>
            {record
              ? `The ${recordTypeName(record.type)} record for ${fqdn(zoneName, record.name)} will stop resolving. This cannot be undone.`
              : null}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <p role="alert" className="text-xs text-destructive">
            {error}
          </p>
        ) : null}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={deleting}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={deleting}
            onClick={(event) => {
              event.preventDefault();
              void handleConfirm();
            }}
          >
            {deleting ? "Deleting…" : "Delete record"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
