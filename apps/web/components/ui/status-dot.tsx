import * as React from "react"

import { cn } from "@/lib/utils"

type StatusDotTone = "success" | "warning" | "muted" | "info" | "destructive"

const toneClassName: Record<StatusDotTone, string> = {
  success: "bg-success",
  warning: "bg-warning",
  muted: "bg-faint",
  info: "bg-primary",
  destructive: "bg-destructive",
}

// The design's halo is the dot's own colour at 20% alpha (it only ever draws the blue
// one). A fixed blue ring around an amber "partial" dot on a warning banner reads as a
// mistake, so the glow follows the tone.
const pulseToneClassName: Record<StatusDotTone, string> = {
  success: "ring-success/20",
  warning: "ring-warning/20",
  muted: "ring-faint/20",
  info: "ring-primary/20",
  destructive: "ring-destructive/20",
}

function StatusDot({
  tone,
  pulse = false,
  label,
  className,
  ...props
}: React.ComponentProps<"span"> & {
  tone: StatusDotTone
  pulse?: boolean
  label?: string
}) {
  return (
    <span
      data-slot="status-dot"
      data-tone={tone}
      aria-hidden={label ? undefined : true}
      className={cn(
        "inline-block size-2 rounded-full",
        toneClassName[tone],
        pulse && [
          "size-2.5 ring-4 motion-safe:animate-pulse",
          pulseToneClassName[tone],
        ],
        className
      )}
      {...props}
    >
      {label ? <span className="sr-only">{label}</span> : null}
    </span>
  )
}

export { StatusDot, type StatusDotTone }
