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

import type {
  ImportZoneResponse,
  ServerStatus,
  Zone,
  ZoneImportMode,
} from "@/gen/dns/v1/dns_pb";
import { getDNSClient } from "@/lib/dns-client";
import { lower, toOwnerName, type RecordDraft } from "@/lib/dns-values";
import { describeError } from "@/lib/errors";

export type ZonesState = {
  /** Always sorted by name; server truth only — mutations are never optimistic. */
  zones: Zone[];
  server?: ServerStatus;
  /** True once the first listZones has settled, successfully or not. */
  loaded: boolean;
  refreshing: boolean;
  /** Date.now() of the last successful list; set on the async path, never in render. */
  loadedAt?: number;
  /** Last load/refresh failure. Mutations throw instead of writing here. */
  error?: string;
};

export type ZonesApi = {
  refresh(): Promise<void>;
  clearError(): void;
  getZone(name: string): Zone | undefined;
  createZone(name: string): Promise<Zone>;
  deleteZone(zoneId: string): Promise<void>;
  createRecord(zoneId: string, draft: RecordDraft): Promise<Zone>;
  updateRecord(
    zoneId: string,
    recordId: string,
    draft: RecordDraft,
  ): Promise<Zone>;
  deleteRecord(zoneId: string, recordId: string): Promise<Zone>;
  importZone(input: {
    name: string;
    zoneFile: string;
    mode: ZoneImportMode;
    dryRun: boolean;
  }): Promise<ImportZoneResponse>;
  exportZone(zoneId: string): Promise<{ name: string; zoneFile: string }>;
};

export type ZonesContextValue = ZonesState & ZonesApi;

type Action =
  | { type: "loadStart" }
  | { type: "loadOk"; zones: Zone[]; server?: ServerStatus; at: number }
  | { type: "loadFail"; message: string }
  | { type: "refreshStart" }
  | { type: "refreshOk"; zones: Zone[]; server?: ServerStatus; at: number }
  | { type: "refreshFail"; message: string }
  | { type: "listCancelled" }
  | { type: "upsertZone"; zone: Zone }
  | { type: "removeZone"; zoneId: string }
  | { type: "clearError" };

const initialState: ZonesState = {
  zones: [],
  loaded: false,
  refreshing: false,
};

const staleAfterMs = 60_000;

function sortZones(zones: Zone[]): Zone[] {
  return [...zones].sort((a, b) => a.name.localeCompare(b.name));
}

function reducer(state: ZonesState, action: Action): ZonesState {
  switch (action.type) {
    case "loadStart":
    case "refreshStart":
      return { ...state, refreshing: true };
    case "loadOk":
    case "refreshOk":
      return {
        zones: sortZones(action.zones),
        server: action.server,
        loaded: true,
        refreshing: false,
        loadedAt: action.at,
        error: undefined,
      };
    case "loadFail":
      return { ...state, loaded: true, refreshing: false, error: action.message };
    case "refreshFail":
      return { ...state, refreshing: false, error: action.message };
    case "listCancelled":
      // A mutation landed while this list was in flight, so its snapshot was dropped
      // and the list is being re-issued. Nothing is marked loaded here: only a list
      // that actually landed may stamp `loaded`/`loadedAt`, otherwise the store would
      // present the mutation's zone alone as if it were the complete list.
      return { ...state, refreshing: false };
    case "upsertZone": {
      const next = state.zones.some((zone) => zone.id === action.zone.id)
        ? state.zones.map((zone) =>
            zone.id === action.zone.id ? action.zone : zone,
          )
        : [...state.zones, action.zone];
      return { ...state, zones: sortZones(next) };
    }
    case "removeZone":
      return {
        ...state,
        zones: state.zones.filter((zone) => zone.id !== action.zoneId),
      };
    case "clearError":
      return { ...state, error: undefined };
    default:
      return state;
  }
}

function requireZone(zone: Zone | undefined): Zone {
  if (!zone) {
    throw new Error("The server returned an empty zone response.");
  }
  return zone;
}

const ZonesContext = createContext<ZonesContextValue | undefined>(undefined);

export function ZonesProvider({
  children,
}: {
  children: ReactNode;
}): JSX.Element {
  const [state, dispatch] = useReducer(reducer, initialState);
  // A monotonically increasing id so an out-of-order list response is dropped.
  const listId = useRef(0);
  // Bumped by every applied mutation. A list request started before a mutation carries a
  // pre-mutation snapshot, so it must not overwrite the zone the mutation just returned.
  const mutationId = useRef(0);
  const { zones, loadedAt } = state;

  const load = useCallback(async (kind: "load" | "refresh") => {
    // Loops until a list lands that no mutation interrupted, so the store converges on
    // the server's full state instead of keeping only the zones mutations returned.
    for (;;) {
      const id = listId.current + 1;
      listId.current = id;
      const mutations = mutationId.current;
      dispatch({ type: kind === "load" ? "loadStart" : "refreshStart" });
      try {
        const response = await getDNSClient().listZones({});
        if (listId.current !== id) {
          return;
        }
        if (mutationId.current !== mutations) {
          // A mutation applied its zone while this list was in flight: the snapshot
          // is stale, so drop it and list again.
          dispatch({ type: "listCancelled" });
          continue;
        }
        dispatch({
          type: kind === "load" ? "loadOk" : "refreshOk",
          zones: response.zones,
          server: response.status,
          at: Date.now(),
        });
        return;
      } catch (error) {
        if (listId.current !== id) {
          return;
        }
        dispatch({
          type: kind === "load" ? "loadFail" : "refreshFail",
          message: describeError(error),
        });
        return;
      }
    }
  }, []);

  const refresh = useCallback(async () => {
    await load("refresh");
  }, [load]);

  useEffect(() => {
    void load("load");
    return () => {
      // Invalidate an in-flight list so a late response cannot land after unmount.
      listId.current += 1;
    };
  }, [load]);

  useEffect(() => {
    function handleVisibility() {
      if (document.hidden) {
        return;
      }
      // §D: refresh only when the last successful list has gone stale. While the first
      // list is still in flight (or failed) `loadedAt` is undefined and this does
      // nothing — the mount effect owns that case, and ApiErrorAlert offers Retry.
      if (loadedAt !== undefined && Date.now() - loadedAt > staleAfterMs) {
        void load("refresh");
      }
    }

    document.addEventListener("visibilitychange", handleVisibility);
    return () =>
      document.removeEventListener("visibilitychange", handleVisibility);
  }, [load, loadedAt]);

  const clearError = useCallback(() => {
    dispatch({ type: "clearError" });
  }, []);

  const getZone = useCallback(
    (name: string) => {
      const target = lower(name);
      return zones.find((zone) => lower(zone.name) === target);
    },
    [zones],
  );

  // §D normalises owner names against the zone name before every record write. An
  // unknown id would normalise against "" and turn a fully-qualified `www.acme.dev.`
  // into the relative label `www.acme.dev` (served as www.acme.dev.acme.dev), so the
  // write fails loudly here instead: mutations throw and the caller shows the message.
  const zoneNameById = useCallback(
    (zoneId: string) => {
      const zone = zones.find((candidate) => candidate.id === zoneId);
      if (!zone) {
        throw new Error("Unknown zone — refresh and try again.");
      }
      return zone.name;
    },
    [zones],
  );

  // Every mutation result is applied through these two so an older list response can
  // never overwrite it (§D: mutations return the full normalised zone).
  const applyZone = useCallback((zone: Zone) => {
    mutationId.current += 1;
    dispatch({ type: "upsertZone", zone });
  }, []);

  const forgetZone = useCallback((zoneId: string) => {
    mutationId.current += 1;
    dispatch({ type: "removeZone", zoneId });
  }, []);

  const createZone = useCallback(
    async (name: string) => {
      const response = await getDNSClient().createZone({ name });
      const zone = requireZone(response.zone);
      applyZone(zone);
      return zone;
    },
    [applyZone],
  );

  const deleteZone = useCallback(
    async (zoneId: string) => {
      await getDNSClient().deleteZone({ zoneId });
      forgetZone(zoneId);
    },
    [forgetZone],
  );

  const createRecord = useCallback(
    async (zoneId: string, draft: RecordDraft) => {
      const zoneName = zoneNameById(zoneId);
      const response = await getDNSClient().createRecord({
        zoneId,
        name: toOwnerName(zoneName, draft.name),
        type: draft.type,
        ttl: draft.ttl,
        value: draft.value,
      });
      const zone = requireZone(response.zone);
      applyZone(zone);
      return zone;
    },
    [applyZone, zoneNameById],
  );

  const updateRecord = useCallback(
    async (zoneId: string, recordId: string, draft: RecordDraft) => {
      const zoneName = zoneNameById(zoneId);
      const response = await getDNSClient().updateRecord({
        zoneId,
        recordId,
        name: toOwnerName(zoneName, draft.name),
        type: draft.type,
        ttl: draft.ttl,
        value: draft.value,
      });
      const zone = requireZone(response.zone);
      applyZone(zone);
      return zone;
    },
    [applyZone, zoneNameById],
  );

  const deleteRecord = useCallback(
    async (zoneId: string, recordId: string) => {
      const response = await getDNSClient().deleteRecord({ zoneId, recordId });
      const zone = requireZone(response.zone);
      applyZone(zone);
      return zone;
    },
    [applyZone],
  );

  const importZone = useCallback(
    async (input: {
      name: string;
      zoneFile: string;
      mode: ZoneImportMode;
      dryRun: boolean;
    }) => {
      const response = await getDNSClient().importZone(input);
      if (!input.dryRun) {
        applyZone(requireZone(response.zone));
      }
      return response;
    },
    [applyZone],
  );

  const exportZone = useCallback(async (zoneId: string) => {
    const response = await getDNSClient().exportZone({ zoneId });
    return { name: response.name, zoneFile: response.zoneFile };
  }, []);

  const value = useMemo<ZonesContextValue>(
    () => ({
      ...state,
      refresh,
      clearError,
      getZone,
      createZone,
      deleteZone,
      createRecord,
      updateRecord,
      deleteRecord,
      importZone,
      exportZone,
    }),
    [
      state,
      refresh,
      clearError,
      getZone,
      createZone,
      deleteZone,
      createRecord,
      updateRecord,
      deleteRecord,
      importZone,
      exportZone,
    ],
  );

  return <ZonesContext value={value}>{children}</ZonesContext>;
}

export function useZones(): ZonesContextValue {
  const value = useContext(ZonesContext);
  if (!value) {
    throw new Error("useZones must be used inside ZonesProvider");
  }
  return value;
}

export function useZone(name: string): { zone?: Zone; loaded: boolean } {
  const { getZone, loaded } = useZones();
  return { zone: getZone(name), loaded };
}
