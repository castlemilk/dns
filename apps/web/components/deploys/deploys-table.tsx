"use client";

import { MoreVertical } from "lucide-react";

import { RelativeTime } from "@/components/app/relative-time";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { StatusPill } from "@/components/ui/status-pill";
import {
  DeployKind,
  DeployPhase,
  TimeSource,
  type Deploy,
} from "@/gen/hosting/v1/hosting_pb";
import { toDate } from "@/lib/format";
import {
  deployPhaseLabel,
  deployPhaseTone,
  formatDuration,
  isDeployActive,
  observedTimesTitle,
  resolvedCommit,
} from "@/lib/platform-model";
import { cn } from "@/lib/utils";

export type DeploysTableProps = {
  deploys: Deploy[];
  /** Zone ids that still have a site: only those can be deployed or rolled back. */
  attachedZoneIds: Set<string>;
  /**
   * Zone names this console holds a Zone for. Every action below needs one, so a row
   * whose domain is missing from the zone list gets disabled items with the reason,
   * rather than menu entries that do nothing when clicked.
   */
  knownZoneNames: Set<string>;
  onShowLog(deploy: Deploy): void;
  onRollback(deploy: Deploy): void;
  onDeployAgain(deploy: Deploy): void;
};

// Written out in full (never interpolated) so Tailwind sees both candidates.
const headerColumns = "grid-cols-[1fr_1.5fr_130px_130px_90px_44px]";
const rowColumns = "md:grid-cols-[1fr_1.5fr_130px_130px_90px_44px]";

const repositoryLabel = (url: string) =>
  url.replace(/^https:\/\/github\.com\//, "").replace(/\.git$/, "");

/** The short half of a deploy id (`<unixms:016x>-<8 hex>`), enough to tell rows apart. */
const shortId = (id: string) => id.split("-").pop() ?? id;

export function deploySourceLabel(deploy: Deploy): string {
  const source = deploy.source;
  if (deploy.kind === DeployKind.ROLLBACK) {
    // `rolled_back_from` is the deploy that WAS live, not the one now serving:
    // an arrow would read as a destination and name the wrong build.
    return deploy.rolledBackFrom
      ? `rollback (replaced ${shortId(deploy.rolledBackFrom)})`
      : "rollback";
  }
  if (deploy.kind === DeployKind.UPLOAD) {
    return source?.uploadName
      ? `folder upload · ${source.uploadName}`
      : "folder upload";
  }
  const repository = source?.repository ? repositoryLabel(source.repository) : "";
  const revision = source?.revision ?? "";
  if (!repository) {
    return revision || "—";
  }
  return revision ? `github · ${repository} @ ${revision}` : `github · ${repository}`;
}

function durationLabel(deploy: Deploy): { text: string; approximate: boolean } {
  if (deploy.buildSeconds === 0) {
    return { text: "—", approximate: false };
  }
  const approximate = deploy.timeSource !== TimeSource.ENGINE;
  return {
    text: `${approximate ? "≈" : ""}${formatDuration(deploy.buildSeconds)}`,
    approximate,
  };
}

/**
 * One row per deploy the control plane recorded, newest first. Every value is a field
 * of the `Deploy` message — a build with no reported duration shows an em dash rather
 * than a plausible number.
 */
export function DeploysTable({
  deploys,
  attachedZoneIds,
  knownZoneNames,
  onShowLog,
  onRollback,
  onDeployAgain,
}: DeploysTableProps) {
  return (
    <div
      role="table"
      aria-label="Deploys"
      className="overflow-hidden rounded-[10px] border border-line"
    >
      <div role="rowgroup">
        <div
          role="row"
          className={cn(
            "eyebrow hidden border-b border-line bg-fill-faint px-5 py-2.5 md:grid",
            headerColumns,
          )}
        >
          <div role="columnheader">Domain</div>
          <div role="columnheader">Source</div>
          <div role="columnheader">Status</div>
          <div role="columnheader">Started</div>
          <div role="columnheader">Duration</div>
          <div role="columnheader">
            <span className="sr-only">Actions</span>
          </div>
        </div>
      </div>

      <div role="rowgroup">
        {deploys.map((deploy) => {
          const started =
            toDate(deploy.startedAt) ?? toDate(deploy.requestedAt);
          const duration = durationLabel(deploy);
          const commit = resolvedCommit(deploy);
          const zoneKnown = knownZoneNames.has(deploy.zoneName);
          const attached = zoneKnown && attachedZoneIds.has(deploy.zoneId);
          const rollbackable =
            attached &&
            deploy.releaseId !== "" &&
            deploy.phase !== DeployPhase.LIVE;
          return (
            <div
              key={deploy.id}
              role="row"
              className={cn(
                "text-ui grid grid-cols-1 gap-1.5 border-b border-line-soft px-5 py-4 last:border-0 hover:bg-row-hover md:items-center md:gap-0",
                rowColumns,
              )}
            >
              <div role="cell" className="flex gap-2.5 md:block">
                <span className="eyebrow w-20 shrink-0 pt-0.5 md:hidden">
                  Domain
                </span>
                <span className="font-medium">{deploy.zoneName}</span>
              </div>

              <div role="cell" className="flex gap-2.5 md:block">
                <span className="eyebrow w-20 shrink-0 pt-0.5 md:hidden">
                  Source
                </span>
                <span className="font-mono text-[13px] break-all text-subtle">
                  {deploySourceLabel(deploy)}
                  {/* Only when the engine resolved one: revision_resolved is
                      source.revision unchanged whenever it reported none. */}
                  {commit ? (
                    <span className="text-muted-foreground" title={commit.full}>
                      {" · "}
                      {commit.short}
                    </span>
                  ) : null}
                </span>
              </div>

              <div role="cell" className="flex gap-2.5 md:block">
                <span className="eyebrow w-20 shrink-0 pt-0.5 md:hidden">
                  Status
                </span>
                <span className="flex flex-col items-start gap-1">
                  <StatusPill
                    tone={deployPhaseTone(deploy.phase)}
                    pulse={isDeployActive(deploy)}
                    aria-label={`Deploy ${deployPhaseLabel(deploy.phase)}`}
                  >
                    {deployPhaseLabel(deploy.phase)}
                  </StatusPill>
                  {deploy.reason ? (
                    <span className="text-xs text-muted-foreground">
                      {deploy.reason}
                    </span>
                  ) : null}
                </span>
              </div>

              <div role="cell" className="flex gap-2.5 md:block">
                <span className="eyebrow w-20 shrink-0 pt-0.5 md:hidden">
                  Started
                </span>
                <RelativeTime
                  date={started}
                  className="font-mono text-[13px] text-muted-foreground"
                />
              </div>

              <div role="cell" className="flex gap-2.5 md:block">
                <span className="eyebrow w-20 shrink-0 pt-0.5 md:hidden">
                  Duration
                </span>
                <span
                  className="font-mono text-[13px] text-muted-foreground"
                  title={duration.approximate ? observedTimesTitle : undefined}
                >
                  {duration.text}
                </span>
              </div>

              <div role="cell" className="md:justify-self-end">
                <DropdownMenu>
                  <DropdownMenuTrigger asChild>
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      aria-label={`Actions for the ${deploy.zoneName} deploy ${deploySourceLabel(deploy)}`}
                    >
                      <MoreVertical aria-hidden="true" className="size-4" />
                    </Button>
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="end">
                    <DropdownMenuItem
                      disabled={!zoneKnown}
                      onSelect={() => onShowLog(deploy)}
                    >
                      {zoneKnown
                        ? "Deploy log"
                        : `Deploy log — ${deploy.zoneName} isn't in this console's domain list`}
                    </DropdownMenuItem>
                    {rollbackable ? (
                      <DropdownMenuItem onSelect={() => onRollback(deploy)}>
                        Roll back to this
                      </DropdownMenuItem>
                    ) : null}
                    {attached ? (
                      <DropdownMenuItem onSelect={() => onDeployAgain(deploy)}>
                        Deploy again
                      </DropdownMenuItem>
                    ) : null}
                  </DropdownMenuContent>
                </DropdownMenu>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
