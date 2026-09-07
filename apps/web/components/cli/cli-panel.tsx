import type { ReactNode } from "react";

import { cn } from "@/lib/utils";

export type CliPanelTone = "info" | "success" | "muted" | "warning";

export type CliPanelProps = {
  tone: CliPanelTone;
  /** A lucide icon element, sized by the caller so it matches the operator gate. */
  icon: ReactNode;
  title: string;
  lead?: ReactNode;
  children?: ReactNode;
  className?: string;
};

const toneClassName: Record<CliPanelTone, string> = {
  info: "bg-primary/10 text-primary",
  success: "bg-success/10 text-success",
  muted: "bg-fill-strong text-muted-foreground",
  warning: "bg-warning/10 text-warning",
};

/**
 * The one card shape the three `/cli` screens share, so the approval, the
 * success and the refusal all read as the same product. It is the operator
 * gate's card with the icon tinted per outcome — no new tokens, no new CSS.
 */
export function CliPanel({
  tone,
  icon,
  title,
  lead,
  children,
  className,
}: CliPanelProps) {
  return (
    <section
      data-slot="cli-panel"
      aria-labelledby="cli-panel-title"
      className={cn(
        "mx-auto flex w-full max-w-[560px] flex-col rounded-xl border border-line bg-card p-7",
        className,
      )}
    >
      <div
        className={cn(
          "flex size-10 items-center justify-center rounded-lg",
          toneClassName[tone],
        )}
      >
        {icon}
      </div>

      <p className="eyebrow mt-5">deephost CLI</p>
      <h1
        id="cli-panel-title"
        className="mt-1.5 text-xl leading-tight font-semibold"
      >
        {title}
      </h1>
      {lead ? (
        <p className="text-ui mt-2 leading-[1.55] text-subtle">{lead}</p>
      ) : null}

      {children}
    </section>
  );
}

/** A short mono run of a command or a path. Never a credential. */
export function CliCode({ children }: { children: ReactNode }) {
  return (
    <code className="rounded-[5px] bg-fill px-1.5 py-0.5 font-mono text-[12.5px] break-all text-soft">
      {children}
    </code>
  );
}
