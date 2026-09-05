"use client";

import { useCallback, useEffect, useRef, useState } from "react";

export type CopyFailure = { value: string };

export type CopyState = {
  /** Copies `value`; `key` identifies the button when several share one hook. */
  copy: (value: string, key?: string) => void;
  copiedKey?: string;
  failure?: CopyFailure;
};

const clearAfterMs = 2000;

/**
 * Clipboard helper that never throws: when the Clipboard API is missing or refuses
 * (insecure origin, denied permission) the failure is reported so the caller can show
 * the value for manual selection. Only the "Copied" flash auto-clears (after two
 * seconds); the failure stays until the next copy() settles, because it is the fallback
 * the user selects the value from and two seconds is not long enough to do that.
 */
export function useCopy(): CopyState {
  const [copiedKey, setCopiedKey] = useState<string>();
  const [failure, setFailure] = useState<CopyFailure>();
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(() => {
    return () => {
      if (timer.current !== undefined) {
        clearTimeout(timer.current);
      }
    };
  }, []);

  const copy = useCallback((value: string, key?: string) => {
    const id = key ?? value;

    function settle(ok: boolean) {
      setCopiedKey(ok ? id : undefined);
      // Replacing the failure on every settle is what clears it: a later successful copy
      // removes the fallback, a later failure re-points it at the new value.
      setFailure(ok ? undefined : { value });
      if (timer.current !== undefined) {
        clearTimeout(timer.current);
        timer.current = undefined;
      }
      if (!ok) {
        // The role="alert" fallback must stay on screen long enough to select the value.
        return;
      }
      timer.current = setTimeout(() => {
        timer.current = undefined;
        setCopiedKey(undefined);
      }, clearAfterMs);
    }

    try {
      const clipboard =
        typeof navigator === "undefined" ? undefined : navigator.clipboard;
      if (!clipboard) {
        settle(false);
        return;
      }
      clipboard.writeText(value).then(
        () => settle(true),
        () => settle(false),
      );
    } catch {
      settle(false);
    }
  }, []);

  return { copy, copiedKey, failure };
}
