import { Skeleton } from "@/components/ui/skeleton";

const rowKeys = ["a", "b", "c", "d", "e", "f"];
const cardKeys = ["attention", "latest", "server"];

/**
 * Placeholder for everything on the dashboard that depends on the zone list. The page
 * title and the header actions are static, so they stay visible while this renders.
 */
export function DashboardSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-live="polite"
      className="flex flex-col gap-7"
    >
      <span className="sr-only">Loading domains…</span>

      <div className="overflow-hidden rounded-[10px] border border-line">
        <div className="border-b border-line bg-fill-faint px-5 py-2.5">
          <Skeleton className="h-3 w-40" />
        </div>
        {rowKeys.map((key) => (
          <div
            key={key}
            className="flex flex-col gap-2.5 border-b border-line-soft px-5 py-4 last:border-0 md:flex-row md:items-center md:gap-6"
          >
            <div className="flex flex-1 items-center gap-3">
              <Skeleton className="size-2 rounded-full" />
              <Skeleton className="h-4 w-40" />
            </div>
            <Skeleton className="h-3.5 w-28 md:flex-1" />
            <Skeleton className="h-3.5 w-24 md:flex-1" />
            <Skeleton className="h-3.5 w-32 md:flex-1" />
            <Skeleton className="h-3.5 w-20 md:w-[100px]" />
          </div>
        ))}
      </div>

      <div className="grid gap-4 md:grid-cols-3">
        {cardKeys.map((key) => (
          <div
            key={key}
            className="flex flex-col gap-2.5 rounded-[10px] border border-line px-5 py-[18px]"
          >
            <Skeleton className="h-3 w-24" />
            <Skeleton className="h-4 w-44" />
            <Skeleton className="h-3 w-32" />
          </div>
        ))}
      </div>
    </div>
  );
}
