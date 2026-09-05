import type { Metadata } from "next";

import { DeploysPage } from "@/components/deploys/deploys-page";
import { normaliseZoneName } from "@/lib/dns-values";

export const metadata: Metadata = { title: "Deploys" };

// Reading searchParams on the server keeps the page out of the useSearchParams
// Suspense requirement (docs: use-search-params.md, "Prerendering"), as /connect does.
export default async function Page(props: PageProps<"/deploys">) {
  const { domain } = await props.searchParams;
  return (
    <DeploysPage
      initialDomain={
        typeof domain === "string" ? normaliseZoneName(domain) : undefined
      }
    />
  );
}
