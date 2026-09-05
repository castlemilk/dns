import type { Metadata } from "next";

import { CliApproved } from "@/components/cli/cli-approved";

export const metadata: Metadata = { title: "CLI authorized" };

export default function Page() {
  return <CliApproved />;
}
