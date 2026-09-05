import type { RecordChange } from "@/gen/platform/v1/platform_pb";
import { cn } from "@/lib/utils";

export type RecordChangeListProps = {
  /** The plan the engine would apply. */
  changes: RecordChange[];
  /** Existing user records the plan cannot keep; empty unless the caller replaces them. */
  conflicts: RecordChange[];
};

type Chip = { label: string; className: string; detail?: string };

const chips: Record<string, Chip> = {
  create: { label: "Add", className: "text-success" },
  adopt: {
    label: "Keep",
    className: "text-muted-foreground",
    detail: "existing record, now automatic",
  },
  align: { label: "Keep", className: "text-muted-foreground" },
  keep: { label: "Keep", className: "text-muted-foreground" },
  remove: { label: "Remove", className: "text-destructive" },
  replace: { label: "Replace", className: "text-warning" },
  // The planner emits "blocked" for a record it could not write because a
  // conflicting record stands in the way. It must never read as applied;
  // change.why carries the sentence naming what is in the way.
  blocked: {
    label: "Blocked",
    className: "text-warning",
    detail: "not written — a conflicting record has to go first",
  },
};

const fallbackChip: Chip = { label: "Change", className: "text-muted-foreground" };

/**
 * The exact record plan the control plane returned for a dry run — one row per change,
 * in the engine's order. Nothing is inferred here: `op`, `why` and `previous_value`
 * come straight off the wire.
 */
export function RecordChangeList({ changes, conflicts }: RecordChangeListProps) {
  if (!changes.length && !conflicts.length) {
    return (
      <p className="text-ui text-muted-foreground">
        Nothing to change — the records are already in place.
      </p>
    );
  }

  return (
    <div className="flex flex-col gap-3">
      {changes.length ? (
        <ul className="flex flex-col gap-1.5 font-mono text-[13px]">
          {changes.map((change, index) => {
            const chip = chips[change.op] ?? fallbackChip;
            const detail = change.why || chip.detail;
            return (
              <li
                key={`${change.op}-${change.type}-${change.name}-${change.value}-${index}`}
                className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5"
              >
                <span className={cn("w-[62px] shrink-0", chip.className)}>
                  {chip.label}
                </span>
                <span className="text-link">{change.type}</span>
                <span>{change.name}</span>
                {change.op === "replace" && change.previousValue ? (
                  <span className="text-muted-foreground line-through">
                    {change.previousValue}
                  </span>
                ) : null}
                <span className="text-soft break-all">{change.value}</span>
                {detail ? (
                  <span className="font-sans text-xs text-muted-foreground">
                    {detail}
                  </span>
                ) : null}
              </li>
            );
          })}
        </ul>
      ) : null}

      {conflicts.length ? (
        <div className="flex flex-col gap-1.5">
          <p className="text-xs font-medium text-warning">
            {conflicts.length === 1
              ? "1 existing record conflicts"
              : `${conflicts.length} existing records conflict`}
          </p>
          <ul className="flex flex-col gap-1.5 font-mono text-[13px] text-warning">
            {conflicts.map((conflict, index) => (
              <li
                key={`conflict-${conflict.type}-${conflict.name}-${conflict.value}-${index}`}
                className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5"
              >
                <span>{conflict.type}</span>
                <span>{conflict.name}</span>
                <span className="break-all">{conflict.value}</span>
                <span className="font-sans text-xs">(custom)</span>
                {conflict.why ? (
                  <span className="font-sans text-xs text-muted-foreground">
                    — {conflict.why}
                  </span>
                ) : null}
              </li>
            ))}
          </ul>
        </div>
      ) : null}
    </div>
  );
}
