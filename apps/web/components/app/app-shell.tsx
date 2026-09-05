"use client";

import { useState, useSyncExternalStore, type ReactNode } from "react";
import { LoaderCircle, LockKeyhole, Menu, RefreshCw } from "lucide-react";

import { ApiErrorAlert } from "@/components/app/api-error-alert";
import { BrandMark } from "@/components/app/brand-mark";
import { OperatorGate } from "@/components/app/operator-gate";
import {
  PlatformProvider,
  usePlatform,
} from "@/components/app/platform-provider";
import { SidebarNav } from "@/components/app/sidebar-nav";
import { useZones, ZonesProvider } from "@/components/app/zones-provider";
import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetTitle,
  SheetTrigger,
} from "@/components/ui/sheet";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  getOperatorSessionSnapshot,
  getServerOperatorSessionSnapshot,
  lockOperatorSession,
  subscribeToOperatorSession,
} from "@/lib/operator-session";
import { cn } from "@/lib/utils";

function MobileTopBar() {
  const [open, setOpen] = useState(false);
  const { refreshing } = useZones();
  const { invalidate } = usePlatform();

  return (
    <header className="sticky top-0 z-30 flex h-14 items-center gap-2 border-b border-line bg-background/90 px-3 backdrop-blur-md lg:hidden">
      <Sheet open={open} onOpenChange={setOpen}>
        <SheetTrigger asChild>
          <Button variant="ghost" size="icon" aria-label="Open navigation">
            <Menu />
          </Button>
        </SheetTrigger>
        <SheetContent
          side="left"
          className="w-[260px] max-w-[88vw] bg-background p-4"
        >
          <SheetTitle className="sr-only">Navigation</SheetTitle>
          <SidebarNav onNavigate={() => setOpen(false)} />
        </SheetContent>
      </Sheet>
      <BrandMark href="/domains" />
      <span className="flex-1" />
      <Button
        variant="ghost"
        size="icon"
        aria-label="Refresh domains"
        disabled={refreshing}
        onClick={() => {
          void invalidate();
        }}
      >
        <RefreshCw className={cn(refreshing && "motion-safe:animate-spin")} />
      </Button>
      <Button
        variant="ghost"
        size="icon"
        aria-label="Lock operator session"
        onClick={() => lockOperatorSession("manual")}
      >
        <LockKeyhole />
      </Button>
    </header>
  );
}

export function AppShell({ children }: { children: ReactNode }) {
  const session = useSyncExternalStore(
    subscribeToOperatorSession,
    getOperatorSessionSnapshot,
    getServerOperatorSessionSnapshot,
  );

  if (session.status === "checking") {
    return (
      <main
        className="flex min-h-dvh items-center justify-center"
        aria-busy="true"
      >
        {/* The spinner is decoration; the sentence beside it is the state, so a
            reader who has turned motion off (and a screen-reader user) loses
            nothing when the rotation is not generated. */}
        <div
          role="status"
          className="text-ui flex items-center gap-2 text-muted-foreground"
        >
          <LoaderCircle
            className="size-4 motion-safe:animate-spin"
            aria-hidden="true"
          />
          Checking operator access…
        </div>
      </main>
    );
  }

  if (session.status === "locked") {
    return <OperatorGate />;
  }

  return (
    <TooltipProvider delayDuration={300}>
      <ZonesProvider>
        {/* PlatformProvider wraps the whole grid, not just <main>: the sidebar (and the
            same nav inside the mobile Sheet) reads the engine status for its links. */}
        <PlatformProvider>
          <div className="min-h-dvh lg:grid lg:grid-cols-[220px_minmax(0,1fr)]">
            <aside className="hidden gap-1.5 border-r border-sidebar-border px-4 py-5 lg:sticky lg:top-0 lg:flex lg:h-dvh lg:flex-col">
              <SidebarNav />
            </aside>
            <div className="flex min-w-0 flex-col">
              <MobileTopBar />
              <main className="mx-auto flex w-full max-w-[1200px] flex-col gap-7 px-4 py-6 sm:px-8 sm:py-8 lg:px-11 lg:py-9">
                <ApiErrorAlert />
                {children}
              </main>
            </div>
          </div>
        </PlatformProvider>
      </ZonesProvider>
    </TooltipProvider>
  );
}
