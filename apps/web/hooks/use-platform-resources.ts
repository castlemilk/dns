"use client";

import { useCallback, useEffect, useMemo, useReducer, useRef } from "react";

import { usePlatform } from "@/components/app/platform-provider";
import type { Event } from "@/gen/activity/v1/activity_pb";
import type { Invoice } from "@/gen/billing/v1/billing_pb";
import {
  LogSource,
  type Deploy,
  type GetDeployLogResponse,
} from "@/gen/hosting/v1/hosting_pb";
import type {
  Forwarder,
  GetMailQueueResponse,
  Mailbox,
} from "@/gen/mail/v1/mail_pb";
import { describeError } from "@/lib/errors";
import {
  getActivityClient,
  getBillingClient,
  getHostingClient,
  getMailClient,
} from "@/lib/platform-client";

/**
 * Page-level resources: the lists the sidebar pages own, kept out of `PlatformProvider`
 * because nothing else needs them and they page. Every request is abortable, stale
 * responses are dropped by request id, and polling only runs while the tab is visible.
 */
export type Resource<T> = {
  items: T[];
  loaded: boolean;
  error?: string;
  nextCursor?: string;
  refresh(): Promise<void>;
  loadMore(): Promise<void>;
};

type Page<T, M = undefined> = {
  items: T[];
  nextCursor?: string;
  meta?: M;
};

type ListState = {
  items: unknown[];
  loaded: boolean;
  error?: string;
  nextCursor?: string;
  meta?: unknown;
};

type ListAction =
  | { type: "reset" }
  | {
      type: "ok";
      items: unknown[];
      nextCursor?: string;
      meta?: unknown;
      append: boolean;
    }
  | { type: "fail"; message: string };

const emptyList: ListState = { items: [], loaded: false };

function listReducer(state: ListState, action: ListAction): ListState {
  switch (action.type) {
    case "reset":
      return emptyList;
    case "ok":
      return {
        items: action.append ? [...state.items, ...action.items] : action.items,
        loaded: true,
        error: undefined,
        nextCursor: action.nextCursor,
        meta: action.meta,
      };
    case "fail":
      return { ...state, loaded: true, error: action.message };
    default:
      return state;
  }
}

/**
 * `fetchPage` must be a stable callback (usually `useCallback` on the ids it reads) and
 * `key` must change whenever the query changes, which resets the list.
 */
function useList<T, M = undefined>(
  enabled: boolean,
  key: string,
  fetchPage: (
    cursor: string | undefined,
    signal: AbortSignal,
  ) => Promise<Page<T, M>>,
): Resource<T> & { meta?: M } {
  const [state, dispatch] = useReducer(listReducer, emptyList);
  const requestId = useRef(0);
  const controller = useRef<AbortController | undefined>(undefined);

  const run = useCallback(
    async (cursor?: string) => {
      const id = requestId.current + 1;
      requestId.current = id;
      controller.current?.abort();
      const abort = new AbortController();
      controller.current = abort;
      try {
        const page = await fetchPage(cursor, abort.signal);
        if (requestId.current !== id) {
          return;
        }
        dispatch({
          type: "ok",
          items: page.items,
          nextCursor: page.nextCursor,
          meta: page.meta,
          append: cursor !== undefined,
        });
      } catch (error) {
        if (requestId.current !== id || abort.signal.aborted) {
          return;
        }
        dispatch({ type: "fail", message: describeError(error) });
      }
    },
    [fetchPage],
  );

  useEffect(() => {
    // `key` is in the dependency list so a changed filter restarts the list; the value
    // itself is not read here.
    void key;
    if (!enabled) {
      dispatch({ type: "reset" });
      return;
    }
    dispatch({ type: "reset" });
    void run();
    return () => {
      requestId.current += 1;
      controller.current?.abort();
    };
  }, [enabled, key, run]);

  const refresh = useCallback(async () => {
    await run();
  }, [run]);

  const nextCursor = state.nextCursor;
  const loadMore = useCallback(async () => {
    if (!nextCursor) {
      return;
    }
    await run(nextCursor);
  }, [nextCursor, run]);

  return useMemo(
    () => ({
      items: state.items as T[],
      loaded: state.loaded,
      error: state.error,
      nextCursor,
      meta: state.meta as M | undefined,
      refresh,
      loadMore,
    }),
    [loadMore, nextCursor, refresh, state.error, state.items, state.loaded, state.meta],
  );
}

/* -------------------------------------------------------------------------- */
/* Single-object resources                                                     */
/* -------------------------------------------------------------------------- */

type SingleState = { value?: unknown; loaded: boolean; error?: string };
type SingleAction =
  | { type: "reset" }
  | { type: "ok"; value: unknown }
  | { type: "fail"; message: string };

const emptySingle: SingleState = { loaded: false };

function singleReducer(state: SingleState, action: SingleAction): SingleState {
  switch (action.type) {
    case "reset":
      return emptySingle;
    case "ok":
      return { value: action.value, loaded: true, error: undefined };
    case "fail":
      return { ...state, loaded: true, error: action.message };
    default:
      return state;
  }
}

function useSingle<T>(
  enabled: boolean,
  key: string,
  fetchOne: (signal: AbortSignal) => Promise<T>,
  poll?: { intervalMs: number; while?: (value: T | undefined) => boolean },
): { value?: T; loaded: boolean; error?: string; refresh(): Promise<void> } {
  const [state, dispatch] = useReducer(singleReducer, emptySingle);
  const requestId = useRef(0);
  const controller = useRef<AbortController | undefined>(undefined);

  const run = useCallback(async () => {
    const id = requestId.current + 1;
    requestId.current = id;
    controller.current?.abort();
    const abort = new AbortController();
    controller.current = abort;
    try {
      const value = await fetchOne(abort.signal);
      if (requestId.current !== id) {
        return;
      }
      dispatch({ type: "ok", value });
    } catch (error) {
      if (requestId.current !== id || abort.signal.aborted) {
        return;
      }
      dispatch({ type: "fail", message: describeError(error) });
    }
  }, [fetchOne]);

  useEffect(() => {
    void key;
    dispatch({ type: "reset" });
    if (!enabled) {
      return;
    }
    void run();
    return () => {
      requestId.current += 1;
      controller.current?.abort();
    };
  }, [enabled, key, run]);

  // `while` is a pure predicate over the current value, so a log that turned out to be
  // a stored snapshot stops polling on the very next render.
  const keepPolling =
    enabled &&
    poll !== undefined &&
    (poll.while ? poll.while(state.value as T | undefined) : true);
  const pollIntervalMs = poll?.intervalMs;

  useEffect(() => {
    if (!keepPolling || !pollIntervalMs) {
      return;
    }
    const tick = () => {
      if (!document.hidden) {
        void run();
      }
    };
    const timer = setInterval(tick, pollIntervalMs);
    return () => clearInterval(timer);
  }, [keepPolling, pollIntervalMs, run]);

  const refresh = useCallback(async () => {
    await run();
  }, [run]);

  return useMemo(
    () => ({
      value: state.value as T | undefined,
      loaded: state.loaded,
      error: state.error,
      refresh,
    }),
    [refresh, state.error, state.loaded, state.value],
  );
}

/* -------------------------------------------------------------------------- */
/* Hosting                                                                     */
/* -------------------------------------------------------------------------- */

const deployPageSize = 20;
const logPollMs = 3_000;

/**
 * Keep asking only while the engine is still streaming: a SNAPSHOT was stored once and
 * NONE will never appear, so polling either of them would be noise.
 */
const keepFollowingLog = (log?: GetDeployLogResponse) =>
  log === undefined || log.source === LogSource.LIVE;

/** Deploys for one zone, or every zone when `zoneId` is omitted. */
export function useDeploys(zoneId?: string): Resource<Deploy> {
  const { hosting } = usePlatform();
  const fetchPage = useCallback(
    async (cursor: string | undefined, signal: AbortSignal) => {
      const response = await getHostingClient().listDeploys(
        { zoneId: zoneId ?? "", limit: deployPageSize, cursor: cursor ?? "" },
        { signal },
      );
      return {
        items: response.deploys,
        nextCursor: response.nextCursor || undefined,
      };
    },
    [zoneId],
  );
  return useList<Deploy>(
    hosting.configured,
    `deploys:${zoneId ?? "*"}`,
    fetchPage,
  );
}

/**
 * The build log. Polls only while the caller says the deploy is still running **and**
 * the last answer came from the engine live — a stored snapshot never changes again.
 */
export function useDeployLog(
  zoneId: string,
  deployId: string,
  opts: { poll: boolean },
): {
  log?: GetDeployLogResponse;
  loaded: boolean;
  error?: string;
  refresh(): Promise<void>;
} {
  const { hosting } = usePlatform();
  const fetchOne = useCallback(
    async (signal: AbortSignal) =>
      getHostingClient().getDeployLog({ zoneId, deployId }, { signal }),
    [deployId, zoneId],
  );
  const state = useSingle<GetDeployLogResponse>(
    hosting.configured && deployId !== "",
    `deploy-log:${zoneId}:${deployId}`,
    fetchOne,
    opts.poll ? { intervalMs: logPollMs, while: keepFollowingLog } : undefined,
  );
  return {
    log: state.value,
    loaded: state.loaded,
    error: state.error,
    refresh: state.refresh,
  };
}

/* -------------------------------------------------------------------------- */
/* Mail                                                                        */
/* -------------------------------------------------------------------------- */

export function useMailboxes(
  zoneId: string,
): Resource<Mailbox> & { limit: number } {
  const { mail } = usePlatform();
  const fetchPage = useCallback(
    async (_cursor: string | undefined, signal: AbortSignal) => {
      const response = await getMailClient().listMailboxes(
        { zoneId },
        { signal },
      );
      return { items: response.mailboxes, meta: response.limit };
    },
    [zoneId],
  );
  const { meta, ...resource } = useList<Mailbox, number>(
    mail.configured && zoneId !== "",
    `mailboxes:${zoneId}`,
    fetchPage,
  );
  return { ...resource, limit: meta ?? 0 };
}

export function useForwarders(zoneId: string): Resource<Forwarder> {
  const { mail } = usePlatform();
  const fetchPage = useCallback(
    async (_cursor: string | undefined, signal: AbortSignal) => {
      const response = await getMailClient().listForwarders(
        { zoneId },
        { signal },
      );
      return { items: response.forwarders };
    },
    [zoneId],
  );
  return useList<Forwarder>(
    mail.configured && zoneId !== "",
    `forwarders:${zoneId}`,
    fetchPage,
  );
}

export function useMailQueue(zoneId: string): {
  queue?: GetMailQueueResponse;
  loaded: boolean;
  error?: string;
  refresh(): Promise<void>;
} {
  const { mail } = usePlatform();
  const fetchOne = useCallback(
    async (signal: AbortSignal) =>
      getMailClient().getMailQueue({ zoneId }, { signal }),
    [zoneId],
  );
  const state = useSingle<GetMailQueueResponse>(
    mail.configured && zoneId !== "",
    `mail-queue:${zoneId}`,
    fetchOne,
  );
  return {
    queue: state.value,
    loaded: state.loaded,
    error: state.error,
    refresh: state.refresh,
  };
}

/* -------------------------------------------------------------------------- */
/* Billing and activity                                                        */
/* -------------------------------------------------------------------------- */

const invoicePageSize = 25;

export function useInvoices(): Resource<Invoice> & { live: boolean } {
  const { billing } = usePlatform();
  const fetchPage = useCallback(
    async (cursor: string | undefined, signal: AbortSignal) => {
      const response = await getBillingClient().listInvoices(
        { limit: invoicePageSize, cursor: cursor ?? "" },
        { signal },
      );
      return {
        items: response.invoices,
        nextCursor: response.nextCursor || undefined,
        meta: response.live,
      };
    },
    [],
  );
  const { meta, ...resource } = useList<Invoice, boolean>(
    billing.configured,
    "invoices",
    fetchPage,
  );
  // `live` is only false once the provider actually answered that way; while the first
  // page is still loading the page shows a skeleton, not "couldn't be loaded".
  return { ...resource, live: meta ?? true };
}

const activityPageSize = 50;

export function useActivity(filter: {
  zoneId?: string;
  kinds?: string[];
}): Resource<Event> {
  const zoneId = filter.zoneId ?? "";
  const kindsKey = (filter.kinds ?? []).join(",");
  const fetchPage = useCallback(
    async (cursor: string | undefined, signal: AbortSignal) => {
      const response = await getActivityClient().listEvents(
        {
          zoneId,
          kinds: kindsKey ? kindsKey.split(",") : [],
          limit: activityPageSize,
          cursor: cursor ?? "",
        },
        { signal },
      );
      return {
        items: response.events,
        nextCursor: response.nextCursor || undefined,
      };
    },
    [kindsKey, zoneId],
  );
  // Activity is served by this control plane itself, so it is never "not configured".
  return useList<Event>(true, `activity:${zoneId}:${kindsKey}`, fetchPage);
}
