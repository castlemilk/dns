import { cn } from "@/lib/utils";

export type DismissGuardNoteProps = {
  /** The guard is in force: Escape and a click outside are being ignored. */
  active: boolean;
  /** What the dialog is doing, as the start of a sentence ("Writing the records"). */
  action: string;
  className?: string;
};

/**
 * The one sentence every dialog that refuses to close mid-write owes its reader.
 *
 * A dialog that swallows Escape and hides its close button is indistinguishable from a
 * broken one unless it says why, and a screen-reader user gets no signal at all that the
 * keyboard stopped working. `role="status"` announces the sentence when the write starts,
 * and the element is dropped again when it finishes, so the guard is only ever described
 * while it is actually in force.
 */
export function DismissGuardNote({
  active,
  action,
  className,
}: DismissGuardNoteProps) {
  if (!active) {
    return null;
  }

  return (
    <p
      role="status"
      data-slot="dismiss-guard-note"
      className={cn("text-xs text-muted-foreground", className)}
    >
      {action} — this dialog stays open until that finishes, so a half-written
      change is never left without its report. Escape and clicking outside are
      ignored until then.
    </p>
  );
}
