import Link from "next/link";

import { cn } from "@/lib/utils";

export type BrandMarkProps = {
  size?: "sm" | "md";
  href?: string;
  className?: string;
};

const markSize = {
  sm: "size-[18px] rounded-[5px]",
  md: "size-[22px] rounded-md",
} as const;

const wordSize = {
  sm: "text-sm",
  md: "text-[15px]",
} as const;

export function BrandMark({ size = "md", href, className }: BrandMarkProps) {
  const content = (
    <>
      <span
        aria-hidden="true"
        className={cn("shrink-0 bg-primary", markSize[size])}
      />
      <span className={cn("font-semibold", wordSize[size])}>simple</span>
    </>
  );

  const classes = cn(
    "inline-flex items-center gap-2.5 text-foreground",
    className,
  );

  if (href) {
    return (
      <Link
        data-slot="brand-mark"
        href={href}
        className={cn(
          classes,
          "rounded-sm outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring",
        )}
      >
        {content}
      </Link>
    );
  }

  return (
    <span data-slot="brand-mark" className={classes}>
      {content}
    </span>
  );
}
