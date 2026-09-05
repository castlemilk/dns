"use client";

import Link from "next/link";

import { useZones } from "@/components/app/zones-provider";
import { Button } from "@/components/ui/button";

export type DomainUnavailableProps = {
  zoneName: string;
};

/**
 * Shown when the domain list never arrived (the first `listZones` failed, so the
 * store settled with no zones at all). Nothing is known about this domain in
 * that state, so the view must not claim it is missing — `DomainNotFound` is
 * only correct once a zone list has actually been received.
 */
export function DomainUnavailable({ zoneName }: DomainUnavailableProps) {
  const { error, refresh, refreshing } = useZones();

  return (
    <div className="flex flex-col items-start gap-3">
      <h1 className="text-xl font-semibold break-all">
        Couldn’t load {zoneName}.
      </h1>
      <p className="text-ui text-subtle">
        The control API did not answer, so it is not known whether this domain is
        here.{error ? ` ${error}` : ""}
      </p>
      <div className="flex flex-wrap items-center gap-4 text-[13px]">
        <Button
          variant="outline"
          size="sm"
          disabled={refreshing}
          onClick={() => {
            void refresh();
          }}
        >
          {refreshing ? "Retrying…" : "Try again"}
        </Button>
        <Link href="/domains" className="link">
          All domains
        </Link>
      </div>
    </div>
  );
}
