import type { Metadata } from "next";

import { CliDenied } from "@/components/cli/cli-denied";

export const metadata: Metadata = { title: "CLI login declined" };

export default function Page() {
  return <CliDenied />;
}
