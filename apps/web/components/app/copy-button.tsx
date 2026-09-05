"use client";

import { useCopy } from "@/hooks/use-copy";
import { cn } from "@/lib/utils";

export type CopyButtonProps = {
  value: string;
  label?: string;
  copiedLabel?: string;
  /** What the value is called in the accessible name, when the value itself is too
   *  long to read out (e.g. "zone file"). Omit for short, single-line values. */
  valueLabel?: string;
  className?: string;
};

/** A whole zone file must never become the button's accessible name. */
function spokenValue(value: string, valueLabel?: string): string | undefined {
  if (valueLabel) {
    return valueLabel;
  }
  return value.length <= 80 && !value.includes("\n") ? value : undefined;
}

export function CopyButton({
  value,
  label = "Copy",
  copiedLabel = "Copied",
  valueLabel,
  className,
}: CopyButtonProps) {
  const { copy, copiedKey, failure } = useCopy();
  const copied = copiedKey === value;
  const spoken = spokenValue(value, valueLabel);

  return (
    <span
      data-slot="copy-button"
      className="inline-flex min-w-0 flex-col items-end gap-1"
    >
      <button
        type="button"
        onClick={() => copy(value)}
        className={cn(
          "link rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring",
          className,
        )}
      >
        {copied ? copiedLabel : label}
        {/* Names the button ("Copy ns1.example.net") without a static aria-label, so the
            name follows the visible text when it flips to "Copied". */}
        {spoken ? <span className="sr-only"> {spoken}</span> : null}
      </button>
      {/* Always mounted so the success message is announced when it appears. */}
      <span aria-live="polite" className="sr-only">
        {copied ? (spoken ? `${copiedLabel} ${spoken}` : copiedLabel) : ""}
      </span>
      {failure ? (
        <span role="alert" className="text-xs text-warning">
          Copying didn&rsquo;t work — select it:{" "}
          <code className="font-mono break-all select-all">
            {failure.value}
          </code>
        </span>
      ) : null}
    </span>
  );
}
