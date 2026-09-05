import * as React from "react"
import { Slot } from "radix-ui"

import { cn } from "@/lib/utils"

function Card({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="card"
      className={cn(
        "flex flex-col rounded-xl border border-line bg-card text-card-foreground",
        className
      )}
      {...props}
    />
  )
}

function CardHeader({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="card-header"
      className={cn(
        "flex items-center justify-between gap-3 border-b border-line-soft px-5 pt-[18px] pb-3.5",
        className
      )}
      {...props}
    />
  )
}

// A real heading: the card titles are the domain view's primary sections, and text
// styled as a heading without heading markup is WCAG 1.3.1 failure F2. The pages that
// use Card put an h1 above it, so h2 is the right level.
function CardTitle({ className, ...props }: React.ComponentProps<"h2">) {
  return (
    <h2
      data-slot="card-title"
      className={cn("text-[15px] font-semibold", className)}
      {...props}
    />
  )
}

function CardAction({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="card-action"
      className={cn("flex shrink-0 items-center gap-2.5", className)}
      {...props}
    />
  )
}

function CardContent({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="card-content"
      className={cn("text-ui flex flex-col gap-3.5 px-5 py-[18px]", className)}
      {...props}
    />
  )
}

function CardFooter({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="card-footer"
      className={cn(
        "mt-auto flex flex-wrap items-center gap-3.5 border-t border-line-soft px-5 py-3.5 text-[13px]",
        className
      )}
      {...props}
    />
  )
}

const cardLinkClassName =
  "link outline-none focus-visible:rounded-sm focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:text-faint"

function CardLink({
  className,
  asChild = false,
  ...props
}: React.ComponentProps<"button"> & { asChild?: boolean }) {
  if (asChild) {
    return (
      <Slot.Root
        data-slot="card-link"
        className={cn(cardLinkClassName, className)}
        {...props}
      />
    )
  }

  return (
    <button
      data-slot="card-link"
      type="button"
      className={cn(cardLinkClassName, className)}
      {...props}
    />
  )
}

export {
  Card,
  CardAction,
  CardContent,
  CardFooter,
  CardHeader,
  CardLink,
  CardTitle,
}
