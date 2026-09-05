import type { Metadata } from "next";

import { ConnectDomainFlow } from "@/components/connect/connect-domain-flow";
import { normaliseZoneName } from "@/lib/dns-values";

export const metadata: Metadata = { title: "Connect a domain" };

// Reading searchParams on the server keeps the flow out of the useSearchParams
// Suspense requirement (docs: use-search-params.md, "Prerendering").
export default async function Page(props: PageProps<"/connect">) {
  const { domain } = await props.searchParams;
  return (
    <ConnectDomainFlow
      initialDomain={
        typeof domain === "string" ? normaliseZoneName(domain) : undefined
      }
    />
  );
}
