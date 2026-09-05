import type { ReactNode } from "react";

import { cn } from "@/lib/utils";

export type PageHeaderProps = {
  eyebrow?: ReactNode;
  title: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
  size?: "md" | "lg";
  className?: string;
};

const titleSize = {
  md: "text-2xl leading-tight font-semibold",
  lg: "text-[30px] leading-[1.1] font-semibold",
} as const;

export function PageHeader({
  eyebrow,
  title,
  subtitle,
  actions,
  size = "md",
  className,
}: PageHeaderProps) {
  return (
    <div
      data-slot="page-header"
      className={cn(
        "flex flex-col gap-4 md:flex-row md:items-end md:justify-between",
        className,
      )}
    >
      <div className="flex min-w-0 flex-col gap-1.5">
        {eyebrow ? <div className="eyebrow">{eyebrow}</div> : null}
        <h1 className={cn("break-words", titleSize[size])}>{title}</h1>
        {subtitle ? (
          <p className="text-ui text-muted-foreground">{subtitle}</p>
        ) : null}
      </div>
      {actions ? (
        <div className="flex flex-wrap items-center gap-2.5">{actions}</div>
      ) : null}
    </div>
  );
}
