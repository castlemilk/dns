import { Skeleton } from "@/components/ui/skeleton";

/** Shown until the first listZones settles; mirrors the domain view's layout. */
export function DomainSkeleton() {
  return (
    <div className="flex flex-col gap-7" aria-busy="true">
      <span className="sr-only" role="status">
        Loading domain…
      </span>
      <div className="flex flex-col gap-4">
        <Skeleton className="h-4 w-40" />
        <div className="flex flex-col gap-4 md:flex-row md:items-end md:justify-between">
          <div className="flex flex-col gap-2.5">
            <Skeleton className="h-8 w-56" />
            <Skeleton className="h-4 w-72" />
          </div>
          <div className="flex gap-2.5">
            <Skeleton className="h-9 w-28 rounded-[7px]" />
            <Skeleton className="h-9 w-24 rounded-[7px]" />
          </div>
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {[0, 1, 2].map((index) => (
          <div
            key={index}
            className="flex flex-col gap-3.5 rounded-xl border border-line bg-card p-5"
          >
            <div className="flex items-center justify-between">
              <Skeleton className="h-4 w-20" />
              <Skeleton className="h-5 w-16 rounded-full" />
            </div>
            <Skeleton className="h-[110px] w-full rounded-lg" />
            <Skeleton className="h-3.5 w-full" />
            <Skeleton className="h-3.5 w-4/5" />
            <Skeleton className="h-3.5 w-2/3" />
          </div>
        ))}
      </div>

      <div className="overflow-hidden rounded-xl border border-line">
        <div className="border-b border-line-soft px-5 py-3.5">
          <Skeleton className="h-4 w-44" />
        </div>
        <div className="flex flex-col gap-3 px-5 py-4">
          {[0, 1, 2, 3, 4, 5].map((index) => (
            <Skeleton key={index} className="h-4 w-full" />
          ))}
        </div>
      </div>
    </div>
  );
}
