import Link from "next/link";

import { Button } from "@/components/ui/button";
import { CardLink } from "@/components/ui/card";

export type DashboardEmptyStateProps = {
  /** Opens the "Add a zone" dialog. Omitted, the secondary link is not rendered. */
  onAddZone?: () => void;
};

export function DashboardEmptyState({ onAddZone }: DashboardEmptyStateProps) {
  return (
    <div className="rounded-[10px] border border-dashed border-line-strong px-6 py-14 text-center">
      <h2 className="text-xl font-semibold">No domains yet</h2>
      <p className="text-ui mx-auto mt-2.5 max-w-sm text-subtle">
        Connect a domain to create its zone here and walk through pointing it,
        the website and email.
      </p>
      <div className="mt-6 flex flex-col items-center gap-3.5">
        <Button
          size="lg"
          className="text-ui rounded-[7px] px-3.5 font-semibold"
          asChild
        >
          <Link href="/connect">Connect a domain</Link>
        </Button>
        {onAddZone ? (
          <CardLink onClick={onAddZone}>Add zone only</CardLink>
        ) : null}
      </div>
    </div>
  );
}
