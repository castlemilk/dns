"use client";

import { useCallback, useEffect, useReducer, useRef } from "react";

import {
  compareNameservers,
  fetchNameserverCheck,
  NameserverCheckError,
  type NameserverCheckResult,
  type NsComparison,
} from "@/lib/nameserver-check";

export type { NsComparison } from "@/lib/nameserver-check";

export type NameserverCheckState = {
  phase: "idle" | "checking" | "checked";
  result?: NameserverCheckResult;
  comparison: NsComparison;
  lastCheckedAt?: Date;
  failure?: string;
  /** The failure cannot be fixed by asking again, so polling has stopped. */
  permanentFailure: boolean;
  failures: number;
  checking: boolean;
  checkNow(): void;
};

type InternalState = {
  phase: "idle" | "checking" | "checked";
  result?: NameserverCheckResult;
  lastCheckedAt?: Date;
  failure?: string;
  permanentFailure: boolean;
  failures: number;
};

type Action =
  | { type: "start" }
  | { type: "ok"; result: NameserverCheckResult; at: Date }
  | { type: "fail"; message: string; permanent: boolean; at: Date }
  | { type: "reset" };

const initialState: InternalState = {
  phase: "idle",
  permanentFailure: false,
  failures: 0,
};

const requestTimeoutMs = 8_000;
const defaultIntervalMs = 30_000;

function reducer(state: InternalState, action: Action): InternalState {
  switch (action.type) {
    case "start":
      return state.phase === "checking" ? state : { ...state, phase: "checking" };
    case "ok":
      return {
        phase: "checked",
        result: action.result,
        lastCheckedAt: action.at,
        failure: undefined,
        permanentFailure: false,
        failures: 0,
      };
    case "fail":
      // The last successful result is kept: a transient network failure should
      // not make a domain that was pointed here look unknown again.
      return {
        phase: "checked",
        result: state.result,
        lastCheckedAt: action.at,
        failure: action.message,
        permanentFailure: action.permanent,
        failures: state.failures + 1,
      };
    case "reset":
      return state === initialState ? state : initialState;
  }
}

function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === "AbortError";
}

function failureMessage(error: unknown): string {
  if (error instanceof DOMException && error.name === "TimeoutError") {
    return "the check timed out";
  }
  if (error instanceof Error && error.message) {
    return error.message;
  }
  return "the check failed";
}

/**
 * Polls /api/nameservers for `domain` and compares the answer with the
 * nameservers this service serves the zone from. Callers derive "last check
 * {n}s ago" from `lastCheckedAt` and their own clock (`useNow`).
 */
export function useNameserverCheck(opts: {
  domain: string;
  expected: string[];
  enabled: boolean;
  poll: boolean;
  intervalMs?: number;
}): NameserverCheckState {
  const {
    domain,
    expected,
    enabled,
    poll,
    intervalMs = defaultIntervalMs,
  } = opts;
  const [state, dispatch] = useReducer(reducer, initialState);
  const controllerRef = useRef<AbortController | null>(null);

  const checkNow = useCallback(() => {
    // This runs from a mount effect, so anything that throws here takes the whole route
    // down (there is no error boundary under app/(app)). `AbortSignal.any`/`.timeout` are
    // newer (Chrome 116 / Firefox 124 / Safari 17.4) than the browsers Next 16 compiles
    // for (Chrome 111 / Safari 16.4), so the 8 s cap is a plain timer instead.
    try {
      controllerRef.current?.abort();
      const controller = new AbortController();
      controllerRef.current = controller;
      let timedOut = false;
      const timeout = setTimeout(() => {
        timedOut = true;
        controller.abort(new DOMException("the check timed out", "TimeoutError"));
      }, requestTimeoutMs);

      dispatch({ type: "start" });
      fetchNameserverCheck(domain, controller.signal).then(
        (result) => {
          clearTimeout(timeout);
          if (controllerRef.current !== controller) {
            return;
          }
          dispatch({ type: "ok", result, at: new Date() });
        },
        (error: unknown) => {
          clearTimeout(timeout);
          if (controllerRef.current !== controller) {
            return;
          }
          // A deliberate abort (superseded check, unmount, `enabled` off) is silent; the
          // timeout aborts too, so it is tracked separately rather than sniffed from the
          // reason — older browsers drop the reason passed to `abort()`.
          if (!timedOut && controller.signal.aborted && isAbortError(error)) {
            return;
          }
          dispatch({
            type: "fail",
            message: timedOut ? "the check timed out" : failureMessage(error),
            permanent:
              !timedOut &&
              error instanceof NameserverCheckError &&
              error.permanent,
            at: new Date(),
          });
        },
      );
    } catch (error) {
      dispatch({
        type: "fail",
        message: failureMessage(error),
        permanent: false,
        at: new Date(),
      });
    }
  }, [domain]);

  // First check on mount, when `enabled` turns true, and whenever the domain
  // changes. `dispatch` (not a `useState` setter) keeps this effect legal.
  useEffect(() => {
    if (!enabled) {
      controllerRef.current?.abort();
      controllerRef.current = null;
      dispatch({ type: "reset" });
      return;
    }

    checkNow();
    return () => {
      controllerRef.current?.abort();
      controllerRef.current = null;
    };
  }, [enabled, checkNow]);

  const comparison = compareNameservers(expected, state.result);
  const pointed = comparison.state === "pointed";

  // Re-check every `intervalMs` until the nameservers point here. The effect
  // re-runs after every settle (the reducer returns a new state object), which
  // is what schedules the next check.
  useEffect(() => {
    // A permanent failure (the name is not one this route looks up) is never
    // retried: polling it would burn a request every 30 s for the same answer.
    if (
      !enabled ||
      !poll ||
      pointed ||
      state.permanentFailure ||
      state.phase !== "checked"
    ) {
      return;
    }

    let handle: ReturnType<typeof setTimeout> | undefined;
    if (!document.hidden) {
      handle = setTimeout(checkNow, intervalMs);
    }

    const onVisibilityChange = () => {
      if (document.hidden) {
        clearTimeout(handle);
        handle = undefined;
      } else if (handle === undefined) {
        checkNow();
      }
    };

    document.addEventListener("visibilitychange", onVisibilityChange);
    return () => {
      clearTimeout(handle);
      document.removeEventListener("visibilitychange", onVisibilityChange);
    };
  }, [enabled, poll, pointed, state, intervalMs, checkNow]);

  return {
    phase: state.phase,
    result: state.result,
    comparison,
    lastCheckedAt: state.lastCheckedAt,
    failure: state.failure,
    permanentFailure: state.permanentFailure,
    failures: state.failures,
    checking: state.phase === "checking",
    checkNow,
  };
}
