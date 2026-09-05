import Link from "next/link";

import { BrandMark } from "@/components/app/brand-mark";

export default function NotFound() {
  return (
    <main className="flex min-h-dvh flex-col items-center justify-center gap-6 bg-page px-6 py-16 text-center">
      <BrandMark href="/" />
      <div className="flex flex-col gap-2">
        <h1 className="text-[30px] leading-[1.1] font-semibold">
          Page not found
        </h1>
        <p className="text-ui max-w-sm text-subtle">
          That address doesn&rsquo;t match anything here.
        </p>
      </div>
      <div className="flex flex-wrap items-center justify-center gap-5 text-[13px]">
        <Link href="/domains" className="link">
          All domains
        </Link>
        <Link href="/" className="text-muted-foreground hover:text-foreground">
          Back to simple
        </Link>
      </div>
    </main>
  );
}
