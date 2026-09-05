import type { Metadata } from "next";

import { MailboxesPage } from "@/components/mailboxes/mailboxes-page";
import { normaliseZoneName } from "@/lib/dns-values";

export const metadata: Metadata = { title: "Mailboxes" };

export default async function Page(props: PageProps<"/mailboxes">) {
  const { domain } = await props.searchParams;
  return (
    <MailboxesPage
      initialDomain={
        typeof domain === "string" ? normaliseZoneName(domain) : undefined
      }
    />
  );
}
