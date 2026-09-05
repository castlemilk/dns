import * as React from "react"

import { cn } from "@/lib/utils"

type StatusPillTone = "success" | "warning" | "muted" | "info"

const toneClassName: Record<StatusPillTone, string> = {
  success: "bg-success/10 text-success",
  warning: "bg-warning/10 text-warning",
  muted: "bg-fill-strong text-muted-foreground",
  info: "bg-primary/10 text-primary",
}

function StatusPill({
  tone,
  pulse = false,
  className,
  ...props
}: React.ComponentProps<"span"> & {
  tone: StatusPillTone
  /** Marks work still in progress (ATTACHING, BUILDING, BINDING). */
  pulse?: boolean
}) {
  return (
    <span
      data-slot="status-pill"
      data-tone={tone}
      className={cn(
        "text-2xs inline-flex items-center rounded-full px-2 py-[3px] font-mono font-medium uppercase",
        toneClassName[tone],
        pulse && "motion-safe:animate-pulse",
        className
      )}
      {...props}
    />
  )
}

export { StatusPill, type StatusPillTone }
