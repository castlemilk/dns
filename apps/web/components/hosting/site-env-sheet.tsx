"use client";

import { SiteEnvPanel } from "@/components/hosting/site-env-panel";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import type { Zone } from "@/gen/dns/v1/dns_pb";

export type SiteEnvSheetProps = {
  zone: Zone;
  open: boolean;
  onOpenChange(open: boolean): void;
};

/**
 * The Environment surface of the Website card: the site's runtime variables per
 * environment, with add and remove. The panel does the work; this only frames
 * it, and unmounts it when closed so no closed sheet holds a request open.
 */
export function SiteEnvSheet({ zone, open, onOpenChange }: SiteEnvSheetProps) {
  if (!open) {
    return null;
  }

  return (
    <Sheet open onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="w-[92vw] overflow-y-auto data-[side=right]:sm:max-w-lg"
      >
        <SheetHeader>
          <SheetTitle>Environment variables</SheetTitle>
          <SheetDescription>
            The values {zone.name} is built and run with. They are sent straight
            to the hosting engine and never shown again, so changing one means
            entering the whole value.
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-3 px-4 pb-4">
          <SiteEnvPanel zoneId={zone.id} zoneName={zone.name} />
        </div>
      </SheetContent>
    </Sheet>
  );
}
