import type { Metadata } from "next";

import { ActivityPage } from "@/components/activity/activity-page";
import { normaliseZoneName } from "@/lib/dns-values";

export const metadata: Metadata = { title: "Activity" };

export default async function Page(props: PageProps<"/activity">) {
  const { domain, kind } = await props.searchParams;
  return (
    <ActivityPage
      initialDomain={
        typeof domain === "string" ? normaliseZoneName(domain) : undefined
      }
      initialKind={typeof kind === "string" ? kind : undefined}
    />
  );
}
