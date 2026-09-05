import type { Metadata } from "next";

import { DomainView } from "@/components/domain/domain-view";
import { normaliseZoneParam } from "@/lib/dns-values";

export async function generateMetadata(
  props: PageProps<"/domains/[zone]">,
): Promise<Metadata> {
  const { zone } = await props.params;
  return { title: normaliseZoneParam(zone) };
}

export default async function Page(props: PageProps<"/domains/[zone]">) {
  const { zone } = await props.params;
  return <DomainView zoneName={normaliseZoneParam(zone)} />;
}
