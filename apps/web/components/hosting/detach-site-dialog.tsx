"use client";

import { useState } from "react";

import { usePlatform } from "@/components/app/platform-provider";
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
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import type { DetachSiteResponse, Site } from "@/gen/hosting/v1/hosting_pb";
import { describeError } from "@/lib/errors";
import { getHostingClient } from "@/lib/platform-client";

export type DetachSiteDialogProps = {
  zone: Zone;
  site: Site;
  /** `capabilities.delete_domain` on the hosting engine status. */
  deleteDomainSupported: boolean;
  open: boolean;
  onOpenChange(open: boolean): void;
  onDone?(result: DetachSiteResponse): void;
};

/**
 * Detaching stops the hosting engine serving this domain. The DNS records it wrote are
 * either deleted or handed back as ordinary custom records — nothing is left behind
 * that the console would still call "automatic".
 */
export function DetachSiteDialog({
  zone,
  site,
  deleteDomainSupported,
  open,
  onOpenChange,
  onDone,
}: DetachSiteDialogProps) {
  const { invalidate } = usePlatform();
  const [records, setRecords] = useState<"keep" | "remove">("remove");
  const [deleteApp, setDeleteApp] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [result, setResult] = useState<DetachSiteResponse>();

  const recordCount = site.dns?.recordIds.length ?? 0;

  async function handleSubmit() {
    setError(undefined);
    setBusy(true);
    try {
      const response = await getHostingClient().detachSite({
        zoneId: zone.id,
        keepDnsRecords: records === "keep",
        keepApp: !deleteApp,
      });
      await invalidate();
      onDone?.(response);
      if (response.pending || response.orphanedHosts.length > 0) {
        // Nothing is finished yet: the engine is still removing hosts, so the dialog
        // stays open with the control plane's own note rather than claiming success.
        setResult(response);
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
          <DialogTitle>Detach the site from {zone.name}?</DialogTitle>
          <DialogDescription>
            {zone.name} stops being served by the hosting engine. Deploy history
            is kept.
          </DialogDescription>
        </DialogHeader>

        {result ? (
          <div className="flex flex-col gap-2">
            <p className="text-ui text-subtle">
              {result.note ||
                "The hosting engine is still removing the hosts; the site finishes detaching in the background."}
            </p>
            {result.orphanedHosts.length ? (
              <p className="text-ui text-warning">
                Still registered:{" "}
                <span className="font-mono text-[13px]">
                  {result.orphanedHosts.join(", ")}
                </span>
              </p>
            ) : null}
          </div>
        ) : (
          <div className="flex flex-col gap-3">
            <fieldset className="flex flex-col gap-2">
              <legend className="text-sm font-medium">
                The {recordCount || ""} automatic DNS{" "}
                {recordCount === 1 ? "record" : "records"}
              </legend>
              <RadioGroup
                value={records}
                onValueChange={(value) =>
                  setRecords(value === "keep" ? "keep" : "remove")
                }
              >
                <div className="flex items-start gap-2">
                  <RadioGroupItem
                    id="detach-remove"
                    value="remove"
                    disabled={busy}
                    className="mt-1"
                  />
                  <Label htmlFor="detach-remove" className="font-normal">
                    Remove them
                  </Label>
                </div>
                <div className="flex items-start gap-2">
                  <RadioGroupItem
                    id="detach-keep"
                    value="keep"
                    disabled={busy}
                    className="mt-1"
                  />
                  <Label htmlFor="detach-keep" className="font-normal">
                    Keep the DNS records as custom records
                  </Label>
                </div>
              </RadioGroup>
            </fieldset>

            <div className="flex items-start gap-2">
              <Checkbox
                id="detach-delete-app"
                checked={deleteApp}
                onCheckedChange={(checked) => setDeleteApp(checked === true)}
                disabled={busy}
                className="mt-0.5"
              />
              <Label htmlFor="detach-delete-app" className="font-normal">
                Also delete the app on the hosting engine
              </Label>
            </div>
            {!deleteApp && !deleteDomainSupported ? (
              <p className="text-xs text-warning">
                This hosting engine version cannot remove hosts from a kept app;
                they stay registered until an operator removes them.
              </p>
            ) : null}

            {error ? (
              <p role="alert" className="text-xs text-destructive">
                {error}
              </p>
            ) : null}
          </div>
        )}

        <DialogFooter>
          {result ? (
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
                variant="destructive"
                onClick={() => {
                  void handleSubmit();
                }}
                disabled={busy}
              >
                {busy ? "Detaching…" : "Detach site"}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
