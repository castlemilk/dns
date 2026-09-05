import Link from "next/link";

import { connectHref, isHostname } from "@/lib/dns-values";

export type DomainNotFoundProps = {
  zoneName: string;
};

export function DomainNotFound({ zoneName }: DomainNotFoundProps) {
  return (
    <div className="flex flex-col items-start gap-3">
      <h1 className="text-xl font-semibold break-all">
        No domain called {zoneName} here.
      </h1>
      <p className="text-ui text-subtle">
        It may have been deleted, or the address is mistyped.
      </p>
      <div className="flex flex-wrap items-center gap-4 text-[13px]">
        <Link href="/domains" className="link">
          All domains
        </Link>
        {isHostname(zoneName) ? (
          <Link
            href={connectHref(zoneName)}
            className="link break-all"
          >
            Connect {zoneName} →
          </Link>
        ) : null}
      </div>
    </div>
  );
}
