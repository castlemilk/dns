"use client";

import { useEffect, useReducer, useRef } from "react";

import type { RecordChange } from "@/gen/platform/v1/platform_pb";
import { describeError } from "@/lib/errors";

/**
 * The dry-run record plan an attach or a bind would apply. Shared by
 * `AttachSiteDialog` and `BindMailDialog`: both ask their facade with `dry_run: true`
 * and render the answer through `RecordChangeList` before anything is written.
 *
 * `useReducer` rather than `useState` because the request is started from an effect and
 * the React Compiler lint rules forbid a synchronous setter call there.
 */
export type RecordPlanResult = {
  dnsPlan: RecordChange[];
  conflicts: RecordChange[];
};

export type RecordPreview = {
  loading: boolean;
  /** The plan settled at least once (successfully or not). */
  loaded: boolean;
  error?: string;
  changes: RecordChange[];
  conflicts: RecordChange[];
};

type Action =
  | { type: "start" }
  | { type: "ok"; plan: RecordPlanResult }
  | { type: "fail"; message: string };

const initialPreview: RecordPreview = {
  loading: false,
  loaded: false,
  changes: [],
  conflicts: [],
};

function reducer(state: RecordPreview, action: Action): RecordPreview {
  switch (action.type) {
    case "start":
      return { ...state, loading: true, error: undefined };
    case "ok":
      return {
        loading: false,
        loaded: true,
        error: undefined,
        changes: action.plan.dnsPlan,
        conflicts: action.plan.conflicts,
      };
    case "fail":
      return { ...state, loading: false, loaded: true, error: action.message };
    default:
      return state;
  }
}

/**
 * `run` must be a stable callback (`useCallback` over the inputs the dry run actually
 * sends) and `key` must change whenever those inputs change — a keystroke in a field
 * the plan does not depend on must not re-ask the control plane.
 */
export function useRecordPreview(
  enabled: boolean,
  key: string,
  run: (signal: AbortSignal) => Promise<RecordPlanResult>,
): RecordPreview {
  const [state, dispatch] = useReducer(reducer, initialPreview);
  const requestId = useRef(0);

  useEffect(() => {
    // `key` is a dependency so a changed input restarts the plan; the value itself is
    // not read here.
    void key;
    if (!enabled) {
      return;
    }
    const id = requestId.current + 1;
    requestId.current = id;
    const abort = new AbortController();
    dispatch({ type: "start" });
    run(abort.signal)
      .then((plan) => {
        if (requestId.current === id) {
          dispatch({ type: "ok", plan });
        }
      })
      .catch((error: unknown) => {
        if (requestId.current === id && !abort.signal.aborted) {
          dispatch({ type: "fail", message: describeError(error) });
        }
      });
    return () => {
      requestId.current += 1;
      abort.abort();
    };
  }, [enabled, key, run]);

  return state;
}
