"use client";

import { useState } from "react";

import { usePlatform } from "@/components/app/platform-provider";
import { RelativeTime } from "@/components/app/relative-time";
import { Button } from "@/components/ui/button";
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
import {
  DeployKind,
  type Deploy,
  type RollbackSiteResponse,
  type Site,
} from "@/gen/hosting/v1/hosting_pb";
import { describeError } from "@/lib/errors";
import { toDate } from "@/lib/format";
import { getHostingClient } from "@/lib/platform-client";
import { buildDurationLabel } from "@/lib/platform-model";

export type RollbackDialogProps = {
  zone: Zone;
  site: Site;
  /** Candidates: superseded deploys that still have a release on the engine. */
  deploys: Deploy[];
  open: boolean;
  onOpenChange(open: boolean): void;
  onDone?(result: RollbackSiteResponse): void;
};

function describeDeploy(deploy: Deploy): string {
  const source = deploy.source;
  if (deploy.kind === DeployKind.UPLOAD) {
    return source?.uploadName ? `folder ${source.uploadName}` : "folder upload";
  }
  return source?.revision || deploy.buildId || deploy.id;
}

/**
 * Rolling back promotes a release the hosting engine already built; nothing is rebuilt
 * and no source is fetched again.
 */
export function RollbackDialog({
  zone,
  site,
  deploys,
  open,
  onOpenChange,
  onDone,
}: RollbackDialogProps) {
  const { invalidate } = usePlatform();
  const candidates = deploys.filter(
    (deploy) => deploy.releaseId !== "" && deploy.id !== site.liveDeployId,
  );
  const [selected, setSelected] = useState(candidates[0]?.id ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function handleSubmit() {
    setError(undefined);
    setBusy(true);
    try {
      const result = await getHostingClient().rollbackSite({
        zoneId: zone.id,
        deployId: selected,
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
          <DialogTitle>Roll back {zone.name}?</DialogTitle>
          <DialogDescription>
            Its release takes traffic within a few seconds; the current one
            becomes superseded and stays available for another rollback.
          </DialogDescription>
        </DialogHeader>

        {candidates.length === 0 ? (
          <p className="text-ui text-muted-foreground">
            There is no earlier release to roll back to.
          </p>
        ) : (
          <RadioGroup
            value={selected}
            onValueChange={setSelected}
            aria-label="Release to roll back to"
          >
            {candidates.map((deploy) => {
              const at =
                toDate(deploy.liveAt) ??
                toDate(deploy.finishedAt) ??
                toDate(deploy.requestedAt);
              const duration = buildDurationLabel(deploy);
              return (
                <div key={deploy.id} className="flex items-start gap-2">
                  <RadioGroupItem
                    id={`rollback-${deploy.id}`}
                    value={deploy.id}
                    disabled={busy}
                    className="mt-1"
                  />
                  <Label
                    htmlFor={`rollback-${deploy.id}`}
                    className="flex-col items-start gap-0.5 font-normal"
                  >
                    <span className="font-mono text-[13px]">
                      {describeDeploy(deploy)}
                    </span>
                    <span className="text-xs text-muted-foreground">
                      <RelativeTime date={at} />
                      {duration ? ` · ${duration}` : ""}
                    </span>
                  </Label>
                </div>
              );
            })}
          </RadioGroup>
        )}

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
            onClick={() => {
              void handleSubmit();
            }}
            disabled={busy || selected === ""}
          >
            {busy ? "Rolling back…" : "Roll back"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
