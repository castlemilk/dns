"use client";

import { useEffect } from "react";

import { usePlatform } from "@/components/app/platform-provider";
import type { Site } from "@/gen/hosting/v1/hosting_pb";
import { isSiteActive } from "@/lib/platform-model";

const defaultIntervalMs = 3_000;

/**
 * Follows a site while the control plane is still working on it: a queued, building or
 * releasing deploy, or a site being attached or detached. Polling stops as soon as the
 * site reaches a settled state and never runs while the tab is hidden — the phases the
 * user cares about are the ones on screen.
 */
export function useDeployPolling(
  site?: Site,
  options?: { intervalMs?: number },
): void {
  const { refreshSite } = usePlatform();
  const zoneId = site?.zoneId;
  const active = isSiteActive(site);
  const intervalMs = options?.intervalMs ?? defaultIntervalMs;

  useEffect(() => {
    if (!active || !zoneId) {
      return;
    }

    const tick = () => {
      if (!document.hidden) {
        void refreshSite(zoneId);
      }
    };

    const timer = setInterval(tick, intervalMs);
    // A tab that comes back after minutes hidden shows the old phase until the next
    // interval; refreshing on the transition closes that gap.
    document.addEventListener("visibilitychange", tick);
    return () => {
      clearInterval(timer);
      document.removeEventListener("visibilitychange", tick);
    };
  }, [active, intervalMs, refreshSite, zoneId]);
}
