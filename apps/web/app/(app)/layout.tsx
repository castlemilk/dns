import type { ReactNode } from "react";

import { AppShell } from "@/components/app/app-shell";

// A route group has no route of its own, so `LayoutProps<"…">` does not apply here.
export default function AppLayout({ children }: { children: ReactNode }) {
  return <AppShell>{children}</AppShell>;
}
