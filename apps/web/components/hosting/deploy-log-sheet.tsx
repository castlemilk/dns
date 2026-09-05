"use client";

import { useEffect, useRef, useState } from "react";

import { CopyButton } from "@/components/app/copy-button";
import { RelativeTime } from "@/components/app/relative-time";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusPill } from "@/components/ui/status-pill";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import { LogSource, type Deploy } from "@/gen/hosting/v1/hosting_pb";
import { useDeployLog } from "@/hooks/use-platform-resources";
import { toDate } from "@/lib/format";
import {
  deployPhaseLabel,
  deployPhaseTone,
  formatDuration,
  isDeployActive,
  resolvedCommit,
} from "@/lib/platform-model";

export type DeployLogSheetProps = {
  zone: Zone;
  deploy: Deploy;
  /** `HOSTING_BUILD_DEADLINE` in seconds, for the "stops waiting after…" copy. */
  buildDeadlineSeconds: number;
  open: boolean;
  onOpenChange(open: boolean): void;
};

/**
 * The build log exactly as the control plane has it: live from the engine while the
 * build runs, the stored snapshot afterwards, and the engine's own note when neither
 * exists. Nothing is reconstructed here.
 */
export function DeployLogSheet({
  zone,
  deploy,
  buildDeadlineSeconds,
  open,
  onOpenChange,
}: DeployLogSheetProps) {
  const active = isDeployActive(deploy);
  // The empty id disables the request: the sheet may stay mounted while closed and a
  // closed sheet must not poll the hosting engine.
  const { log, loaded, error, refresh } = useDeployLog(
    zone.id,
    open ? deploy.id : "",
    { poll: active && open },
  );
  const [follow, setFollow] = useState(true);
  const paneRef = useRef<HTMLPreElement | null>(null);
  const lineCount = log?.lines.length ?? 0;

  useEffect(() => {
    if (!follow) {
      return;
    }
    const pane = paneRef.current;
    if (pane) {
      pane.scrollTop = pane.scrollHeight;
    }
  }, [follow, lineCount]);

  if (!open) {
    return null;
  }

  const commit = resolvedCommit(deploy);
  const capturedAt = toDate(log?.capturedAt);
  const note = log?.note ?? "";
  const text = (log?.lines ?? []).join("\n");

  return (
    <Sheet open onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="w-[92vw] data-[side=right]:sm:max-w-3xl"
      >
        <SheetHeader>
          <SheetTitle className="flex flex-wrap items-center gap-2">
            Deploy log
            <StatusPill
              tone={deployPhaseTone(deploy.phase)}
              pulse={active}
              aria-label={`Deploy ${deployPhaseLabel(deploy.phase)}`}
            >
              {deployPhaseLabel(deploy.phase)}
            </StatusPill>
          </SheetTitle>
          <SheetDescription>
            {zone.name}
            {deploy.source?.revision ? ` · ${deploy.source.revision}` : ""}
            {/* The commit the engine actually built, when it reported one; the
                requested revision alone otherwise, exactly as before. */}
            {commit ? (
              <span className="font-mono" title={commit.full}>
                {" @ "}
                {commit.short}
              </span>
            ) : null}
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-3 px-4 pb-4">
          {/* The log pane itself stays aria-live="off" (see below), so the phase is
              what gets announced: a reader who opened the sheet on a running build
              learns it finished, and whether it succeeded, without polling the pill
              by hand. */}
          <span role="status" className="sr-only">
            Deploy {deployPhaseLabel(deploy.phase).toLowerCase()}
            {deploy.reason ? `. ${deploy.reason}` : ""}
          </span>

          {active ? (
            <p className="text-xs text-muted-foreground">
              Builds can&apos;t be cancelled. The hosting engine enforces its own
              build deadline; this control plane stops waiting after{" "}
              {formatDuration(buildDeadlineSeconds)}.
            </p>
          ) : null}

          {log && log.source !== LogSource.LIVE && note ? (
            <p className="text-ui rounded-[10px] border border-line bg-fill-faint px-3.5 py-2.5 text-subtle">
              {note}
            </p>
          ) : null}

          {error ? (
            <p role="alert" className="text-xs text-destructive">
              {error}
            </p>
          ) : null}

          {!loaded ? (
            <div className="flex flex-col gap-1.5">
              <Skeleton className="h-4 w-2/3" />
              <Skeleton className="h-4 w-1/2" />
              <Skeleton className="h-4 w-3/4" />
            </div>
          ) : lineCount === 0 ? (
            <p className="text-ui text-muted-foreground">
              {log?.source === LogSource.NONE
                ? "No log is available for this build."
                : "The hosting engine hasn't written any output yet."}
            </p>
          ) : (
            <pre
              ref={paneRef}
              // Announcing every streamed line would flood a screen reader; the Follow
              // toggle keeps the newest line in view instead.
              aria-live="off"
              tabIndex={0}
              className="max-h-[70dvh] flex-1 overflow-auto rounded-[10px] border border-line bg-fill-faint px-3.5 py-3 font-mono text-[12.5px] leading-[1.6] whitespace-pre-wrap text-soft outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              {text}
            </pre>
          )}

          <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
            <div className="flex items-center gap-2">
              <Checkbox
                id="deploy-log-follow"
                checked={follow}
                onCheckedChange={(checked) => setFollow(checked === true)}
              />
              <Label htmlFor="deploy-log-follow" className="font-normal">
                Follow
              </Label>
            </div>
            {text ? (
              <CopyButton
                value={text}
                label="Copy log"
                valueLabel="deploy log"
                className="text-[13px]"
              />
            ) : null}
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                void refresh();
              }}
            >
              Refresh
            </Button>
            <span className="text-xs text-muted-foreground">
              {log?.source === LogSource.SNAPSHOT && capturedAt ? (
                <>
                  Snapshot <RelativeTime date={capturedAt} />
                </>
              ) : null}
              {log?.truncated ? " · truncated" : ""}
            </span>
          </div>
        </div>
      </SheetContent>
    </Sheet>
  );
}
