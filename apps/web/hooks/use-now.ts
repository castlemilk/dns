"use client";

import { useEffect, useState } from "react";

/**
 * The only sanctioned clock for render-time relative text: `Date.now()` in a render
 * body fails the react-hooks/purity lint rule, while `new Date()` inside a lazy
 * `useState` initialiser is allowed. The interval is paused while the tab is hidden
 * and catches up as soon as it becomes visible again.
 */
export function useNow(intervalMs: number): Date {
  const [now, setNow] = useState(() => new Date());

  useEffect(() => {
    if (!Number.isFinite(intervalMs) || intervalMs <= 0) {
      return;
    }

    let timer: ReturnType<typeof setInterval> | undefined;

    function tick() {
      setNow(new Date());
    }

    function start() {
      if (timer === undefined) {
        timer = setInterval(tick, intervalMs);
      }
    }

    function stop() {
      if (timer !== undefined) {
        clearInterval(timer);
        timer = undefined;
      }
    }

    function handleVisibility() {
      if (document.hidden) {
        stop();
        return;
      }
      tick();
      start();
    }

    if (!document.hidden) {
      start();
    }
    document.addEventListener("visibilitychange", handleVisibility);

    return () => {
      stop();
      document.removeEventListener("visibilitychange", handleVisibility);
    };
  }, [intervalMs]);

  return now;
}
