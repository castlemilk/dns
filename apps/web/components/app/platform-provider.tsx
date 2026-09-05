"use client";

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useReducer,
  useRef,
  type JSX,
  type ReactNode,
} from "react";
import { Code, ConnectError } from "@connectrpc/connect";

import { useZones } from "@/components/app/zones-provider";
import type {
  ConfirmCheckoutResponse,
  DomainBilling,
  GetBillingStatusResponse,
  GetBillingSummaryResponse,
  Price,
} from "@/gen/billing/v1/billing_pb";
import type {
  GetHostingStatusResponse,
  Site,
} from "@/gen/hosting/v1/hosting_pb";
import type { GetMailStatusResponse, MailDomain } from "@/gen/mail/v1/mail_pb";
import {
  EngineKind,
  type EngineStatus,
  type GetPlatformStatusResponse,
} from "@/gen/platform/v1/platform_pb";
import { describeError } from "@/lib/errors";
import {
  getBillingClient,
  getHostingClient,
  getMailClient,
  getPlatformClient,
} from "@/lib/platform-client";

/**
 * The engine half of the data layer, beside `ZonesProvider` rather than inside it: an
 * unreachable hosting engine must never stop the zone list from rendering, so each
 * engine has its own error channel and its own loading state.
 *
 * Rules (as spec.md §D): one `useReducer`, no optimistic updates — every mutation RPC
 * returns the full object and the caller upserts it here — request ids drop stale
 * responses, and nothing reads the clock in a render body.
 *
 * Frozen contract note for WP4/5/6: three states, not two. Every page that renders an
 * engine must branch
 *
 *   1. `statusError && !status` → the platform status call itself failed; render an
 *      inline `EngineUnreachable` with `refreshStatus(true)` as Retry. Nothing is known
 *      about any engine here, so "isn't configured" would be a false statement (§8.1,
 *      §12.14). Such a slice reports `loaded: false` and carries `statusError` on its
 *      own `error` channel.
 *   2. `!engine.loaded` → skeleton.
 *   3. `!engine.configured` → `EngineNotConfigured` with the engine's `missingEnv`.
 */

export type EngineSlice<T> = {
  configured: boolean;
  loaded: boolean;
  error?: string;
  items: Map<string /* zoneId */, T>;
};

export type PlatformState = {
  status?: GetPlatformStatusResponse;
  statusLoaded: boolean;
  statusError?: string;
  hosting: EngineSlice<Site> & { status?: GetHostingStatusResponse };
  mail: EngineSlice<MailDomain> & { status?: GetMailStatusResponse };
  billing: EngineSlice<DomainBilling> & {
    status?: GetBillingStatusResponse;
    summary?: GetBillingSummaryResponse;
    price?: Price;
  };
  activeDeploys: number;
};

export type PlatformApi = {
  refreshStatus(probe?: boolean): Promise<void>;
  refreshHosting(): Promise<void>;
  refreshMail(): Promise<void>;
  refreshBilling(): Promise<void>;
  refreshSite(zoneId: string): Promise<Site | undefined>;
  refreshMailDomain(zoneId: string): Promise<MailDomain | undefined>;
  confirmCheckout(sessionId: string): Promise<ConfirmCheckoutResponse>;
  engine(kind: EngineKind): EngineStatus | undefined;
  /** After any platform mutation: the three engine lists and the zone list. */
  invalidate(): Promise<void>;
};

export type PlatformContextValue = PlatformState & PlatformApi;

/* -------------------------------------------------------------------------- */
/* State                                                                       */
/* -------------------------------------------------------------------------- */

type Slice<T, S> = {
  configured: boolean;
  /** The list request settled (ok or error) at least once. */
  listSettled: boolean;
  error?: string;
  items: Map<string, T>;
  status?: S;
};

type State = {
  status?: GetPlatformStatusResponse;
  statusLoaded: boolean;
  statusLoadedAt?: number;
  statusError?: string;
  hosting: Slice<Site, GetHostingStatusResponse>;
  mail: Slice<MailDomain, GetMailStatusResponse>;
  billing: Slice<DomainBilling, GetBillingStatusResponse> & {
    summary?: GetBillingSummaryResponse;
  };
};

type Action =
  | { type: "statusOk"; status: GetPlatformStatusResponse; at: number }
  | { type: "statusFail"; message: string }
  | {
      type: "hostingOk";
      status: GetHostingStatusResponse;
      configured: boolean;
      sites: Site[];
    }
  | { type: "hostingFail"; message: string }
  | {
      type: "mailOk";
      status: GetMailStatusResponse;
      configured: boolean;
      domains: MailDomain[];
    }
  | { type: "mailFail"; message: string }
  | {
      type: "billingOk";
      status: GetBillingStatusResponse;
      configured: boolean;
      summary?: GetBillingSummaryResponse;
    }
  | { type: "billingFail"; message: string }
  | { type: "upsertSite"; site: Site }
  | { type: "removeSite"; zoneId: string }
  | { type: "upsertMailDomain"; domain: MailDomain }
  | { type: "removeMailDomain"; zoneId: string }
  | { type: "upsertBilling"; row: DomainBilling };

function emptySlice<T, S>(): Slice<T, S> {
  return { configured: false, listSettled: false, items: new Map<string, T>() };
}

const initialState: State = {
  statusLoaded: false,
  hosting: emptySlice<Site, GetHostingStatusResponse>(),
  mail: emptySlice<MailDomain, GetMailStatusResponse>(),
  billing: emptySlice<DomainBilling, GetBillingStatusResponse>(),
};

const staleAfterMs = 60_000;

function indexBy<T extends { zoneId: string }>(items: T[]): Map<string, T> {
  const map = new Map<string, T>();
  for (const item of items) {
    map.set(item.zoneId, item);
  }
  return map;
}

function withItem<T extends { zoneId: string }, S>(
  slice: Slice<T, S>,
  item: T,
): Slice<T, S> {
  const items = new Map(slice.items);
  items.set(item.zoneId, item);
  return { ...slice, items, error: undefined };
}

function withoutItem<T, S>(slice: Slice<T, S>, zoneId: string): Slice<T, S> {
  if (!slice.items.has(zoneId)) {
    return slice;
  }
  const items = new Map(slice.items);
  items.delete(zoneId);
  return { ...slice, items };
}

function configuredIn(
  status: GetPlatformStatusResponse | undefined,
  kind: EngineKind,
): boolean {
  return (
    status?.engines.find((engine) => engine.kind === kind)?.configured ?? false
  );
}

function reducer(state: State, action: Action): State {
  switch (action.type) {
    case "statusOk":
      return {
        ...state,
        status: action.status,
        statusLoaded: true,
        statusLoadedAt: action.at,
        statusError: undefined,
        // The platform status is the authority on what is configured; an engine that
        // is off keeps `listSettled` false but is still `loaded` (see `expose`).
        hosting: {
          ...state.hosting,
          configured: configuredIn(action.status, EngineKind.HOSTING),
        },
        mail: {
          ...state.mail,
          configured: configuredIn(action.status, EngineKind.MAIL),
        },
        billing: {
          ...state.billing,
          configured: configuredIn(action.status, EngineKind.BILLING),
        },
      };
    case "statusFail":
      // `status` is deliberately left as it was: a failed refresh after a good
      // load keeps the engines the console already knows about, and a failure
      // with no status at all leaves every slice unloaded (see `expose`).
      return { ...state, statusLoaded: true, statusError: action.message };
    case "hostingOk":
      return {
        ...state,
        hosting: {
          configured: action.configured,
          listSettled: true,
          error: undefined,
          items: indexBy(action.sites),
          status: action.status,
        },
      };
    case "hostingFail":
      return {
        ...state,
        hosting: { ...state.hosting, listSettled: true, error: action.message },
      };
    case "mailOk":
      return {
        ...state,
        mail: {
          configured: action.configured,
          listSettled: true,
          error: undefined,
          items: indexBy(action.domains),
          status: action.status,
        },
      };
    case "mailFail":
      return {
        ...state,
        mail: { ...state.mail, listSettled: true, error: action.message },
      };
    case "billingOk":
      return {
        ...state,
        billing: {
          configured: action.configured,
          listSettled: true,
          error: undefined,
          items: indexBy(action.summary?.domains ?? []),
          status: action.status,
          summary: action.summary,
        },
      };
    case "billingFail":
      return {
        ...state,
        billing: { ...state.billing, listSettled: true, error: action.message },
      };
    case "upsertSite":
      return { ...state, hosting: withItem(state.hosting, action.site) };
    case "removeSite":
      return { ...state, hosting: withoutItem(state.hosting, action.zoneId) };
    case "upsertMailDomain":
      return { ...state, mail: withItem(state.mail, action.domain) };
    case "removeMailDomain":
      return { ...state, mail: withoutItem(state.mail, action.zoneId) };
    case "upsertBilling":
      return { ...state, billing: withItem(state.billing, action.row) };
    default:
      return state;
  }
}

/**
 * A configured engine is only "loaded" once its list settled; an unconfigured one is
 * loaded as soon as the platform status says so. Pages render skeletons while
 * `!loaded`, so an unconfigured engine never flashes an empty table and a configured
 * one never flashes "isn't configured".
 *
 * Frozen contract, and the reason `status` is passed in as well as `statusLoaded`:
 * when `GetPlatformStatus` itself failed and no status was ever received, nothing is
 * known about any engine — `configured` is still its initial `false`. Reporting that
 * as `loaded` would make every page state "Hosting isn't configured", which is a
 * false statement about a timed-out call. Such a slice stays `loaded: false` and
 * carries `statusError` on its own error channel, so WP4/5/6 pages branch on
 * `statusError` (inline `EngineUnreachable` + Retry) before `!configured`.
 */
function expose<T, S>(
  slice: Slice<T, S>,
  statusLoaded: boolean,
  statusKnown: boolean,
  statusError?: string,
) {
  return {
    configured: slice.configured,
    loaded: statusLoaded && statusKnown && (!slice.configured || slice.listSettled),
    error: slice.error ?? (statusKnown ? undefined : statusError),
    items: slice.items,
    status: slice.status,
  };
}

const notFound = (error: unknown) =>
  ConnectError.from(error).code === Code.NotFound;

const PlatformContext = createContext<PlatformContextValue | undefined>(
  undefined,
);

export function PlatformProvider({
  children,
}: {
  children: ReactNode;
}): JSX.Element {
  const [state, dispatch] = useReducer(reducer, initialState);
  const { refresh: refreshZones } = useZones();

  const statusId = useRef(0);
  const hostingId = useRef(0);
  const mailId = useRef(0);
  const billingId = useRef(0);
  // Last reconcile stamp seen per zone: when the site DNS reconciler wrote records the
  // zone list is stale, so the zone store is refreshed once instead of on every poll.
  const reconciled = useRef(new Map<string, string>());

  const refreshHosting = useCallback(async () => {
    const id = hostingId.current + 1;
    hostingId.current = id;
    try {
      const client = getHostingClient();
      const status = await client.getHostingStatus({});
      const configured = status.engine?.configured ?? false;
      const sites = configured ? (await client.listSites({})).sites : [];
      if (hostingId.current !== id) {
        return;
      }
      dispatch({ type: "hostingOk", status, configured, sites });
    } catch (error) {
      if (hostingId.current !== id) {
        return;
      }
      dispatch({ type: "hostingFail", message: describeError(error) });
    }
  }, []);

  const refreshMail = useCallback(async () => {
    const id = mailId.current + 1;
    mailId.current = id;
    try {
      const client = getMailClient();
      const status = await client.getMailStatus({});
      const configured = status.engine?.configured ?? false;
      const domains = configured
        ? (await client.listMailDomains({})).domains
        : [];
      if (mailId.current !== id) {
        return;
      }
      dispatch({ type: "mailOk", status, configured, domains });
    } catch (error) {
      if (mailId.current !== id) {
        return;
      }
      dispatch({ type: "mailFail", message: describeError(error) });
    }
  }, []);

  const refreshBilling = useCallback(async () => {
    const id = billingId.current + 1;
    billingId.current = id;
    try {
      const client = getBillingClient();
      const status = await client.getBillingStatus({});
      const configured = status.engine?.configured ?? false;
      const summary = configured
        ? await client.getBillingSummary({})
        : undefined;
      if (billingId.current !== id) {
        return;
      }
      dispatch({ type: "billingOk", status, configured, summary });
    } catch (error) {
      if (billingId.current !== id) {
        return;
      }
      dispatch({ type: "billingFail", message: describeError(error) });
    }
  }, []);

  const refreshStatus = useCallback(
    async (probe = false) => {
      const id = statusId.current + 1;
      statusId.current = id;
      let status: GetPlatformStatusResponse;
      try {
        status = await getPlatformClient().getPlatformStatus({ probe });
      } catch (error) {
        if (statusId.current === id) {
          dispatch({ type: "statusFail", message: describeError(error) });
        }
        return;
      }
      if (statusId.current !== id) {
        return;
      }
      dispatch({ type: "statusOk", status, at: Date.now() });

      // Each engine's list is loaded from here, and only when that engine is
      // configured — an unconfigured engine costs no request at all.
      const pending: Array<Promise<void>> = [];
      if (configuredIn(status, EngineKind.HOSTING)) {
        pending.push(refreshHosting());
      }
      if (configuredIn(status, EngineKind.MAIL)) {
        pending.push(refreshMail());
      }
      if (configuredIn(status, EngineKind.BILLING)) {
        pending.push(refreshBilling());
      }
      await Promise.all(pending);
    },
    [refreshBilling, refreshHosting, refreshMail],
  );

  const refreshSite = useCallback(
    async (zoneId: string) => {
      try {
        const response = await getHostingClient().getSite({ zoneId });
        const site = response.site;
        if (!site) {
          dispatch({ type: "removeSite", zoneId });
          return undefined;
        }
        dispatch({ type: "upsertSite", site });
        const stamp = site.dns?.reconciledAt
          ? `${site.dns.reconciledAt.seconds}.${site.dns.reconciledAt.nanos}`
          : "";
        const previous = reconciled.current.get(zoneId);
        reconciled.current.set(zoneId, stamp);
        if (previous !== undefined && previous !== stamp) {
          void refreshZones();
        }
        return site;
      } catch (error) {
        if (notFound(error)) {
          dispatch({ type: "removeSite", zoneId });
          return undefined;
        }
        dispatch({ type: "hostingFail", message: describeError(error) });
        return undefined;
      }
    },
    [refreshZones],
  );

  const refreshMailDomain = useCallback(async (zoneId: string) => {
    try {
      const response = await getMailClient().getMailDomain({ zoneId });
      const domain = response.domain;
      if (!domain) {
        dispatch({ type: "removeMailDomain", zoneId });
        return undefined;
      }
      dispatch({ type: "upsertMailDomain", domain });
      return domain;
    } catch (error) {
      if (notFound(error)) {
        dispatch({ type: "removeMailDomain", zoneId });
        return undefined;
      }
      dispatch({ type: "mailFail", message: describeError(error) });
      return undefined;
    }
  }, []);

  const confirmCheckout = useCallback(async (sessionId: string) => {
    const response = await getBillingClient().confirmCheckout({ sessionId });
    if (response.domain) {
      dispatch({ type: "upsertBilling", row: response.domain });
    }
    return response;
  }, []);

  const invalidate = useCallback(async () => {
    await Promise.all([
      refreshHosting(),
      refreshMail(),
      refreshBilling(),
      refreshZones(),
    ]);
  }, [refreshBilling, refreshHosting, refreshMail, refreshZones]);

  const engine = useCallback(
    (kind: EngineKind) =>
      state.status?.engines.find((entry) => entry.kind === kind),
    [state.status],
  );

  useEffect(() => {
    void refreshStatus();
  }, [refreshStatus]);

  const { statusLoadedAt } = state;
  useEffect(() => {
    function handleVisibility() {
      if (document.hidden) {
        return;
      }
      if (statusLoadedAt !== undefined && Date.now() - statusLoadedAt > staleAfterMs) {
        void refreshStatus();
      }
    }

    document.addEventListener("visibilitychange", handleVisibility);
    return () =>
      document.removeEventListener("visibilitychange", handleVisibility);
  }, [refreshStatus, statusLoadedAt]);

  const value = useMemo<PlatformContextValue>(() => {
    const statusKnown = state.status !== undefined;
    const billing = expose(
      state.billing,
      state.statusLoaded,
      statusKnown,
      state.statusError,
    );
    return {
      status: state.status,
      statusLoaded: state.statusLoaded,
      statusError: state.statusError,
      hosting: expose(
        state.hosting,
        state.statusLoaded,
        statusKnown,
        state.statusError,
      ),
      mail: expose(
        state.mail,
        state.statusLoaded,
        statusKnown,
        state.statusError,
      ),
      billing: {
        ...billing,
        summary: state.billing.summary,
        price: state.billing.status?.price,
      },
      activeDeploys: state.hosting.status?.activeDeploys ?? 0,
      refreshStatus,
      refreshHosting,
      refreshMail,
      refreshBilling,
      refreshSite,
      refreshMailDomain,
      confirmCheckout,
      engine,
      invalidate,
    };
  }, [
    state,
    refreshStatus,
    refreshHosting,
    refreshMail,
    refreshBilling,
    refreshSite,
    refreshMailDomain,
    confirmCheckout,
    engine,
    invalidate,
  ]);

  return <PlatformContext value={value}>{children}</PlatformContext>;
}

export function usePlatform(): PlatformContextValue {
  const value = useContext(PlatformContext);
  if (!value) {
    throw new Error("usePlatform must be used inside PlatformProvider");
  }
  return value;
}

export function useSite(zoneId?: string): {
  site?: Site;
  configured: boolean;
  loaded: boolean;
  error?: string;
} {
  const { hosting } = usePlatform();
  return {
    site: zoneId ? hosting.items.get(zoneId) : undefined,
    configured: hosting.configured,
    loaded: hosting.loaded,
    error: hosting.error,
  };
}

export function useMailDomain(zoneId?: string): {
  domain?: MailDomain;
  configured: boolean;
  loaded: boolean;
  error?: string;
} {
  const { mail } = usePlatform();
  return {
    domain: zoneId ? mail.items.get(zoneId) : undefined,
    configured: mail.configured,
    loaded: mail.loaded,
    error: mail.error,
  };
}

export function useDomainBilling(zoneId?: string): {
  row?: DomainBilling;
  configured: boolean;
  loaded: boolean;
  error?: string;
} {
  const { billing } = usePlatform();
  return {
    row: zoneId ? billing.items.get(zoneId) : undefined,
    configured: billing.configured,
    loaded: billing.loaded,
    error: billing.error,
  };
}
