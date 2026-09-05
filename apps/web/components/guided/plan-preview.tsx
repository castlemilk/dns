import type { PlanLine } from "@/lib/record-plan";
import { cn } from "@/lib/utils";

export type PlanPreviewProps = {
  lines: PlanLine[];
  notes: string[];
  progress?: { done: number; total: number };
  failure?: string;
  className?: string;
};

const kindLabel: Record<PlanLine["kind"], string> = {
  create: "Add",
  update: "Replace",
  delete: "Remove",
};

const kindClassName: Record<PlanLine["kind"], string> = {
  create: "text-success",
  update: "text-warning",
  delete: "text-destructive",
};

/**
 * Exactly what the guided forms will write, rendered from the plan itself — so the
 * preview cannot drift from the requests that follow.
 */
export function PlanPreview({
  lines,
  notes,
  progress,
  failure,
  className,
}: PlanPreviewProps) {
  return (
    <div
      data-slot="plan-preview"
      className={cn(
        "flex flex-col gap-2.5 rounded-[10px] border border-line bg-fill-faint px-4 py-3.5",
        className,
      )}
    >
      <p className="eyebrow">Records to write</p>

      {lines.length === 0 ? (
        <p className="text-ui text-muted-foreground">
          Nothing to change — these records already exist.
        </p>
      ) : (
        <ul className="flex flex-col gap-1.5">
          {lines.map((line, index) => (
            <li
              key={`${line.kind}-${line.text}-${index}`}
              className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 font-mono text-[13px]"
            >
              <span
                className={cn(
                  "shrink-0 font-sans text-xs font-medium",
                  kindClassName[line.kind],
                )}
              >
                {kindLabel[line.kind]}
              </span>
              <span className="break-all text-soft">{line.text}</span>
              {line.detail ? (
                <span className="font-sans text-xs text-muted-foreground">
                  {line.detail}
                </span>
              ) : null}
            </li>
          ))}
        </ul>
      )}

      {notes.map((note) => (
        <p key={note} className="text-xs text-muted-foreground">
          {note}
        </p>
      ))}

      {progress && !failure ? (
        <p role="status" className="text-xs text-muted-foreground">
          Wrote {progress.done} of {progress.total}…
        </p>
      ) : null}

      {failure ? (
        <p role="alert" className="text-xs text-destructive">
          {progress
            ? `Wrote ${progress.done} of ${progress.total} records, then failed: ${failure}. The domain shows what was written; retry to apply the rest.`
            : `${failure} Nothing was written.`}
        </p>
      ) : null}
    </div>
  );
}
