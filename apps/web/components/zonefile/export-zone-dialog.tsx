"use client";

import { useEffect, useReducer } from "react";

import { CopyButton } from "@/components/app/copy-button";
import { useZones } from "@/components/app/zones-provider";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import { describeError } from "@/lib/errors";

export type ExportZoneDialogProps = {
  open: boolean;
  zone: Zone;
  onOpenChange: (open: boolean) => void;
};

type State = {
  status: "idle" | "loading" | "ready" | "error";
  zoneFile?: string;
  error?: string;
  /** Object URL for the download anchor; revoked when the dialog closes. */
  downloadUrl?: string;
};

type Action =
  | { type: "reset" }
  | { type: "start" }
  | { type: "ok"; zoneFile: string }
  | { type: "fail"; message: string }
  | { type: "downloadUrl"; url?: string };

const initialState: State = { status: "idle" };

function reducer(state: State, action: Action): State {
  switch (action.type) {
    case "reset":
      // Returning the same reference when already reset keeps the close effect from
      // re-rendering forever.
      return state.status === "idle" && state.downloadUrl === undefined
        ? state
        : initialState;
    case "start":
      return { status: "loading" };
    case "ok":
      return { status: "ready", zoneFile: action.zoneFile };
    case "fail":
      return { status: "error", error: action.message };
    case "downloadUrl":
      return state.downloadUrl === action.url
        ? state
        : { ...state, downloadUrl: action.url };
    default:
      return state;
  }
}

export function ExportZoneDialog({
  open,
  zone,
  onOpenChange,
}: ExportZoneDialogProps) {
  const { exportZone } = useZones();
  const [state, dispatch] = useReducer(reducer, initialState);
  const zoneId = zone.id;
  const fileName = `${zone.name}.zone`;

  useEffect(() => {
    if (!open) {
      dispatch({ type: "reset" });
      return;
    }
    let active = true;
    dispatch({ type: "start" });
    exportZone(zoneId).then(
      (response) => {
        if (active) {
          dispatch({ type: "ok", zoneFile: response.zoneFile });
        }
      },
      (error: unknown) => {
        if (active) {
          dispatch({ type: "fail", message: describeError(error) });
        }
      },
    );
    return () => {
      active = false;
    };
  }, [open, zoneId, exportZone]);

  const { zoneFile } = state;
  useEffect(() => {
    if (zoneFile === undefined) {
      return;
    }
    const url = URL.createObjectURL(
      new Blob([zoneFile], { type: "text/plain;charset=utf-8" }),
    );
    dispatch({ type: "downloadUrl", url });
    return () => {
      URL.revokeObjectURL(url);
      dispatch({ type: "downloadUrl", url: undefined });
    };
  }, [zoneFile]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Export zone file</DialogTitle>
          <DialogDescription>
            BIND format, as served — SOA and NS included.
          </DialogDescription>
        </DialogHeader>

        {state.status === "error" ? (
          <p role="alert" className="text-ui text-destructive">
            {state.error}
          </p>
        ) : state.status === "ready" ? (
          <>
            <label htmlFor="export-zone-file" className="sr-only">
              {`Zone file for ${zone.name}`}
            </label>
            <Textarea
              id="export-zone-file"
              mono
              readOnly
              spellCheck={false}
              value={state.zoneFile}
              className="h-72 resize-none select-all"
            />
          </>
        ) : (
          <div className="flex flex-col gap-2" aria-busy="true">
            <Skeleton className="h-72 w-full" />
            <span className="sr-only">Loading the zone file…</span>
          </div>
        )}

        <DialogFooter className="sm:justify-between">
          <span className="flex items-center gap-4 text-[13px]">
            {state.status === "ready" && state.zoneFile !== undefined ? (
              <CopyButton
                value={state.zoneFile}
                label="Copy"
                valueLabel="zone file"
              />
            ) : null}
          </span>
          <span className="flex items-center gap-2">
            <Button variant="outline" onClick={() => onOpenChange(false)}>
              Close
            </Button>
            {state.downloadUrl ? (
              <Button asChild>
                <a href={state.downloadUrl} download={fileName}>
                  {`Download ${fileName}`}
                </a>
              </Button>
            ) : (
              <Button disabled>{`Download ${fileName}`}</Button>
            )}
          </span>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
