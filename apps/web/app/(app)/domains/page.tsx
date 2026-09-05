import type { Metadata } from "next";

import { DomainsDashboard } from "@/components/domains/domains-dashboard";

export const metadata: Metadata = { title: "Domains" };

export default function Page() {
  return <DomainsDashboard />;
}
