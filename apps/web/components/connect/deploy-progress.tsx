import type { ReactNode } from "react";
import type { Timestamp } from "@bufbuild/protobuf/wkt";

import { RelativeTime } from "@/components/app/relative-time";
import { StatusPill } from "@/components/ui/status-pill";
import { DeployPhase, type Deploy } from "@/gen/hosting/v1/hosting_pb";
import { toDate } from "@/lib/format";
import {
  buildDurationLabel,
  deployPhaseLabel,
  deployPhaseTone,
  isDeployActive,
  observedTimesTitle,
} from "@/lib/platform-model";
import { TimeSource } from "@/gen/hosting/v1/hosting_pb";

export type DeployProgressProps = {
  deploy: Deploy;
  /** Static replacement for the deploy-time `RelativeTime` (used by the landing frame). */
  deployedLabel?: ReactNode;
};

/**
 * One line about the deploy the control plane is watching. Pure by contract: every value
 * is read off the message, the clock only enters through `RelativeTime` (or the caller's
 * frozen `deployedLabel`), and a phase the engine has not reached is never anticipated —
 * a queued build says "queued", not "building".
 */
export function DeployProgress({ deploy, deployedLabel }: DeployProgressProps) {
  const label = deployPhaseLabel(deploy.phase);
  const duration = buildDurationLabel(deploy);
  const approximate = duration !== undefined && deploy.timeSource !== TimeSource.ENGINE;

  const at = (ts?: Timestamp) =>
    deployedLabel ?? <RelativeTime date={toDate(ts)} />;

  return (
    <div
      data-slot="deploy-progress"
      className="flex flex-col gap-2 rounded-[10px] border border-line bg-fill-faint px-4 py-3.5"
    >
      {/* The connect flow polls this block: queued -> building -> live happens with
          the reader's hands off the keyboard. The visible sentence below carries the
          detail; this region carries the phase, so the outcome is spoken once each
          time it changes rather than never. */}
      <span role="status" className="sr-only">
        Deploy {label.toLowerCase()}
        {deploy.reason ? `. ${deploy.reason}` : ""}
      </span>

      <div className="flex items-center gap-2.5">
        <StatusPill
          tone={deployPhaseTone(deploy.phase)}
          pulse={isDeployActive(deploy)}
          aria-label={label}
        >
          {label}
        </StatusPill>
        {deploy.source?.repository ? (
          <span className="truncate font-mono text-[13px] text-muted-foreground">
            {deploy.source.repository}
            {deploy.source.revision ? ` @ ${deploy.source.revision}` : ""}
          </span>
        ) : deploy.source?.uploadName ? (
          <span className="truncate font-mono text-[13px] text-muted-foreground">
            {deploy.source.uploadName}
          </span>
        ) : null}
      </div>

      <p className="text-ui text-soft">
        {deploy.phase === DeployPhase.QUEUED ? (
          <>Queued · requested {at(deploy.requestedAt)}</>
        ) : deploy.phase === DeployPhase.BUILDING ||
          deploy.phase === DeployPhase.RELEASING ? (
          <>
            Building… started {at(deploy.startedAt ?? deploy.requestedAt)}. You can
            keep going; the domain page shows progress.
          </>
        ) : deploy.phase === DeployPhase.LIVE ||
          deploy.phase === DeployPhase.SUPERSEDED ? (
          <>
            Live {at(deploy.liveAt ?? deploy.finishedAt)}
            {duration ? ` · ${duration}` : ""}
          </>
        ) : deploy.phase === DeployPhase.FAILED ? (
          <>
            Failed {at(deploy.finishedAt)}
            {deploy.reason ? ` · ${deploy.reason}` : ""}
          </>
        ) : deploy.phase === DeployPhase.ABANDONED ? (
          <>
            The hosting engine did not finish this build
            {deploy.reason ? ` · ${deploy.reason}` : ""}
          </>
        ) : deploy.phase === DeployPhase.LOST ? (
          <>The hosting engine no longer reports this build.</>
        ) : (
          <>Requested {at(deploy.requestedAt)}</>
        )}
      </p>

      {approximate ? (
        <p className="text-xs text-muted-foreground" title={observedTimesTitle}>
          observed by the control plane
        </p>
      ) : null}
    </div>
  );
}
