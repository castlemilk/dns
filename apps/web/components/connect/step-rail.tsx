import { Fragment } from "react";

import { cn } from "@/lib/utils";

export type StepRailStep = { key: string; label: string };

export type StepRailProps = {
  steps: StepRailStep[];
  current: string;
  skipped: string[];
  className?: string;
};

/**
 * The connect flow's progress rail. A step is "done" only when it was passed without
 * being skipped — a skipped step keeps saying so, because nothing was written for it.
 */
export function StepRail({ steps, current, skipped, className }: StepRailProps) {
  const currentIndex = steps.findIndex((step) => step.key === current);

  return (
    <ol
      data-slot="step-rail"
      className={cn(
        "flex flex-wrap items-center gap-2.5 font-mono text-xs font-medium text-muted-foreground",
        className,
      )}
    >
      {steps.map((step, index) => {
        const isCurrent = step.key === current;
        const wasSkipped = skipped.includes(step.key);
        const isDone = currentIndex >= 0 && index < currentIndex && !wasSkipped;

        return (
          <Fragment key={step.key}>
            {index > 0 ? <li aria-hidden="true">—</li> : null}
            <li
              aria-current={isCurrent ? "step" : undefined}
              className={cn(
                isDone && "text-success",
                isCurrent && "text-foreground",
              )}
            >
              {isDone ? `✓ ${step.label}` : step.label}
              {wasSkipped && !isCurrent ? " (skipped)" : ""}
            </li>
          </Fragment>
        );
      })}
    </ol>
  );
}
