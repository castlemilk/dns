"use client";

import { useCallback, useEffect, useMemo, useReducer, useRef } from "react";

import type { EnvVar } from "@/gen/hosting/v1/hosting_pb";
import { describeError } from "@/lib/errors";
import { getHostingClient } from "@/lib/platform-client";

/**
 * The environment variables of one site, exactly as `ListSiteEnvVars` answers.
 *
 * Two things this hook must not lose. `supported` is false when the hosting
 * engine version has no environment-variable RPCs at all: the list is then
 * empty because nothing could be asked, not because the site has none, and the
 * caller renders `note` instead of an empty state. And there is no value here
 * or anywhere else — `EnvVar` has no value field, so nothing a variable holds
 * ever reaches the browser and an "edit" is always a re-entry of the whole
 * value.
 *
 * `useReducer` rather than `useState` because the request starts from an effect,
 * where the React Compiler lint rules forbid a synchronous setter call.
 */
export type SiteEnvVars = {
  variables: EnvVar[];
  /** False when this hosting engine version cannot store variables at all. */
  supported: boolean;
  /** The facade's own copy: the version statement, or the redeploy sentence. */
  note: string;
  /** The list settled at least once (successfully or not). */
  loaded: boolean;
  error?: string;
  refresh(): Promise<void>;
};

type State = {
  variables: EnvVar[];
  supported: boolean;
  note: string;
  loaded: boolean;
  error?: string;
};

type Action =
  | { type: "reset" }
  | { type: "ok"; variables: EnvVar[]; supported: boolean; note: string }
  | { type: "fail"; message: string };

// `supported` starts true so a sheet that has not answered yet renders its
// skeleton rather than flashing "this engine cannot store variables".
const initial: State = {
  variables: [],
  supported: true,
  note: "",
  loaded: false,
};

function reducer(state: State, action: Action): State {
  switch (action.type) {
    case "reset":
      return initial;
    case "ok":
      return {
        variables: action.variables,
        supported: action.supported,
        note: action.note,
        loaded: true,
        error: undefined,
      };
    case "fail":
      return { ...state, loaded: true, error: action.message };
    default:
      return state;
  }
}

export function useSiteEnvVars(
  enabled: boolean,
  zoneId: string,
): SiteEnvVars {
  const [state, dispatch] = useReducer(reducer, initial);
  const requestId = useRef(0);
  const controller = useRef<AbortController | undefined>(undefined);

  const run = useCallback(async () => {
    const id = requestId.current + 1;
    requestId.current = id;
    controller.current?.abort();
    const abort = new AbortController();
    controller.current = abort;
    try {
      // No environment filter: the sheet shows every environment at once, and
      // one request is cheaper than three.
      const response = await getHostingClient().listSiteEnvVars(
        { zoneId, environment: "" },
        { signal: abort.signal },
      );
      if (requestId.current !== id) {
        return;
      }
      dispatch({
        type: "ok",
        variables: response.variables,
        supported: response.supported,
        note: response.note,
      });
    } catch (error) {
      if (requestId.current !== id || abort.signal.aborted) {
        return;
      }
      dispatch({ type: "fail", message: describeError(error) });
    }
  }, [zoneId]);

  useEffect(() => {
    dispatch({ type: "reset" });
    if (!enabled || zoneId === "") {
      return;
    }
    void run();
    return () => {
      requestId.current += 1;
      controller.current?.abort();
    };
  }, [enabled, run, zoneId]);

  const refresh = useCallback(async () => {
    await run();
  }, [run]);

  return useMemo(
    () => ({
      variables: state.variables,
      supported: state.supported,
      note: state.note,
      loaded: state.loaded,
      error: state.error,
      refresh,
    }),
    [
      refresh,
      state.error,
      state.loaded,
      state.note,
      state.supported,
      state.variables,
    ],
  );
}
