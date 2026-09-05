"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useId, useReducer, useRef, type ChangeEvent } from "react";
import { Code, ConnectError } from "@connectrpc/connect";

import { useZones } from "@/components/app/zones-provider";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  ZoneImportMode,
  type Record as DNSRecord,
  type Zone,
} from "@/gen/dns/v1/dns_pb";
import { isHostname, lower, normaliseZoneName, zoneHref } from "@/lib/dns-values";
import { describeError } from "@/lib/errors";
import { plural } from "@/lib/format";

export type ImportZoneDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  mode: "create" | "replace";
  /** The zone being replaced; required in "replace" mode. */
  zone?: Zone;
  onImported: (zone: Zone) => void;
};

/** Client-side guard; the server accepts up to 64 MB. */
const maxFileBytes = 1_000_000;

type Diff = { added: number; removed: number; kept: number };

type Preview = {
  /** The exact name and text the dry run was made against. */
  name: string;
  zoneFile: string;
  warnings: string[];
  userRecords: number;
  diff?: Diff;
};

type State = {
  domain: string;
  text: string;
  preview?: Preview;
  busy: "idle" | "previewing" | "applying";
  domainError?: string;
  /** Name of an existing zone, rendered with a link to it. */
  existingZone?: string;
  /**
   * Zone name the server rejected with AlreadyExists in create mode. The cause is
   * resolved at render time against the (just refreshed) zone list — see
   * `resolveAlreadyExists`.
   */
  alreadyExists?: string;
  fileError?: string;
  formError?: string;
};

type Errors = Pick<
  State,
  "domainError" | "existingZone" | "alreadyExists" | "fileError" | "formError"
>;

type Action =
  | { type: "reset" }
  | { type: "setDomain"; value: string }
  | { type: "setText"; value: string }
  | { type: "start"; busy: "previewing" | "applying" }
  | { type: "previewOk"; preview: Preview }
  | { type: "fail"; errors: Errors };

const initialState: State = { domain: "", text: "", busy: "idle" };

const noErrors: Errors = {
  domainError: undefined,
  existingZone: undefined,
  alreadyExists: undefined,
  fileError: undefined,
  formError: undefined,
};

function reducer(state: State, action: Action): State {
  switch (action.type) {
    case "reset":
      // Same reference when already pristine, so the close effect cannot loop.
      return state === initialState ? state : initialState;
    case "setDomain":
      // Any edit invalidates the previous dry run.
      return {
        ...state,
        ...noErrors,
        domain: action.value,
        preview: undefined,
      };
    case "setText":
      return { ...state, ...noErrors, text: action.value, preview: undefined };
    case "start":
      return { ...state, ...noErrors, busy: action.busy };
    case "previewOk":
      return { ...state, ...noErrors, busy: "idle", preview: action.preview };
    case "fail":
      return { ...state, ...action.errors, busy: "idle" };
    default:
      return state;
  }
}

const recordKey = (record: DNSRecord) =>
  `${lower(record.name)}|${record.type}|${record.value}`;

/**
 * Multiset diff of the non-managed records, keyed by (name, type, value). SOA and apex
 * NS stay managed by the control plane through an import, so they are never counted.
 */
function diffRecords(current: DNSRecord[], next: DNSRecord[]): Diff {
  const remaining = new Map<string, number>();
  for (const record of current) {
    if (record.managed) {
      continue;
    }
    const key = recordKey(record);
    remaining.set(key, (remaining.get(key) ?? 0) + 1);
  }

  let added = 0;
  let kept = 0;
  for (const record of next) {
    if (record.managed) {
      continue;
    }
    const key = recordKey(record);
    const count = remaining.get(key) ?? 0;
    if (count > 0) {
      remaining.set(key, count - 1);
      kept += 1;
    } else {
      added += 1;
    }
  }

  let removed = 0;
  for (const count of remaining.values()) {
    removed += count;
  }
  return { added, removed, kept };
}

/**
 * Error routing verified against handler.ImportZone / store.ImportZone: the handler
 * wraps every zonefile.Parse failure (NormalizeName included) as
 * `zone_file: name: …`, while store-side failures from buildImportedZone arrive
 * without the `zone_file:` prefix.
 */
function routeError(
  error: unknown,
  mode: "create" | "replace",
  zoneName: string,
): Errors {
  const message = describeError(error);
  const connect = ConnectError.from(error);

  const zoneFileName = /^zone_file:\s*name:\s*/.exec(message);
  if (zoneFileName) {
    const detail = message.slice(zoneFileName[0].length);
    return mode === "create"
      ? { ...noErrors, domainError: detail }
      : { ...noErrors, fileError: detail };
  }

  const fieldPrefix = /^(?:zone_file|type|ttl|value|records|name):\s*/.exec(
    message,
  );
  if (fieldPrefix) {
    return { ...noErrors, fileError: message.slice(fieldPrefix[0].length) };
  }

  if (connect.code === Code.AlreadyExists) {
    // CREATE returns the identical code and message ("zone or record already exists")
    // whether the zone name is taken (store.go: checked first, even on a dry run) or
    // the file repeats a record, and the client-side getZone() guard only sees the
    // zone list this tab last loaded. Defer the decision to resolveAlreadyExists,
    // which re-reads the list after a refresh.
    return mode === "create"
      ? { ...noErrors, alreadyExists: zoneName }
      : { ...noErrors, fileError: "The file contains the same record twice." };
  }
  if (connect.code === Code.NotFound && mode === "replace") {
    return { ...noErrors, formError: "This zone no longer exists." };
  }
  return { ...noErrors, formError: message };
}

/**
 * Turns a create-mode AlreadyExists into the message that matches reality: the zone name
 * is taken (actionable — open that zone and import there) when the refreshed list now
 * contains it, otherwise the file repeats a record. `known` is the current
 * `getZone(name)` result, so the message upgrades on its own if a later refresh brings
 * the zone in.
 */
function resolveAlreadyExists(
  state: State,
  known: boolean,
): { existingZone?: string; fileError?: string } {
  if (state.alreadyExists === undefined) {
    return { existingZone: state.existingZone, fileError: state.fileError };
  }
  return known
    ? { existingZone: state.alreadyExists, fileError: state.fileError }
    : {
        existingZone: state.existingZone,
        fileError:
          state.fileError ?? "The file contains the same record twice.",
      };
}

export function ImportZoneDialog({
  open,
  onOpenChange,
  mode,
  zone,
  onImported,
}: ImportZoneDialogProps) {
  const { getZone, importZone, refresh } = useZones();
  const router = useRouter();
  const [state, dispatch] = useReducer(reducer, initialState);
  const requestId = useRef(0);
  const fieldId = useId();

  const domainInputId = `${fieldId}-domain`;
  const domainMessageId = `${fieldId}-domain-message`;
  const textId = `${fieldId}-zone-file`;
  const textMessageId = `${fieldId}-zone-file-message`;
  const fileInputId = `${fieldId}-file`;

  useEffect(() => {
    if (!open) {
      // Also invalidate anything in flight, so a late response cannot leave an error
      // behind for the next time the dialog opens.
      requestId.current += 1;
      dispatch({ type: "reset" });
    }
  }, [open]);

  const zoneName = mode === "create" ? normaliseZoneName(state.domain) : (zone?.name ?? "");
  const importMode =
    mode === "create" ? ZoneImportMode.CREATE : ZoneImportMode.REPLACE;
  const busy = state.busy !== "idle";
  const previewed =
    state.preview !== undefined &&
    state.preview.zoneFile === state.text &&
    state.preview.name === zoneName;
  const { existingZone, fileError } = resolveAlreadyExists(
    state,
    state.alreadyExists !== undefined && getZone(state.alreadyExists) !== undefined,
  );

  function handleFailure(id: number, error: unknown) {
    if (requestId.current !== id) {
      return;
    }
    const errors = routeError(error, mode, zoneName);
    if (errors.alreadyExists !== undefined) {
      // The zone list this tab holds may predate a zone created in another tab or by
      // another operator, so ask the server for a current one before blaming the file.
      // The dialog stays busy until it lands: the message is decided once, not flipped
      // under the user.
      void refresh()
        .catch(() => undefined)
        .then(() => {
          if (requestId.current !== id) {
            return;
          }
          dispatch({ type: "fail", errors });
        });
      return;
    }
    dispatch({ type: "fail", errors });
  }

  function handleFile(event: ChangeEvent<HTMLInputElement>) {
    const input = event.currentTarget;
    const file = input.files?.[0];
    // Clear the input so choosing the same file twice fires another change event.
    input.value = "";
    if (!file) {
      return;
    }
    if (file.size > maxFileBytes) {
      dispatch({
        type: "fail",
        errors: {
          ...noErrors,
          fileError: "That file is larger than 1 MB — paste the records instead.",
        },
      });
      return;
    }
    file.text().then(
      (text) => dispatch({ type: "setText", value: text }),
      () =>
        dispatch({
          type: "fail",
          errors: {
            ...noErrors,
            fileError: "That file could not be read — paste the records instead.",
          },
        }),
    );
  }

  function validate(): Errors | undefined {
    if (mode === "create") {
      if (!isHostname(zoneName)) {
        return {
          ...noErrors,
          domainError: "Enter a bare domain such as studio.photos.",
        };
      }
      if (getZone(zoneName)) {
        return { ...noErrors, existingZone: zoneName };
      }
    } else if (!zone) {
      return { ...noErrors, formError: "This zone no longer exists." };
    }
    if (!state.text.trim()) {
      return {
        ...noErrors,
        fileError: "Paste a zone file or choose one to upload.",
      };
    }
    return undefined;
  }

  function handlePreview() {
    const problem = validate();
    if (problem) {
      dispatch({ type: "fail", errors: problem });
      return;
    }
    const id = requestId.current + 1;
    requestId.current = id;
    const zoneFile = state.text;
    dispatch({ type: "start", busy: "previewing" });
    importZone({ name: zoneName, zoneFile, mode: importMode, dryRun: true }).then(
      (response) => {
        if (requestId.current !== id) {
          return;
        }
        const records = response.zone?.records ?? [];
        dispatch({
          type: "previewOk",
          preview: {
            name: zoneName,
            zoneFile,
            warnings: response.warnings,
            userRecords: records.filter((record) => !record.managed).length,
            diff:
              mode === "replace" && zone
                ? diffRecords(zone.records, records)
                : undefined,
          },
        });
      },
      (error: unknown) => handleFailure(id, error),
    );
  }

  function handleApply() {
    const problem = validate();
    if (problem) {
      dispatch({ type: "fail", errors: problem });
      return;
    }
    const id = requestId.current + 1;
    requestId.current = id;
    const zoneFile = state.text;
    dispatch({ type: "start", busy: "applying" });
    importZone({ name: zoneName, zoneFile, mode: importMode, dryRun: false }).then(
      (response) => {
        if (requestId.current !== id) {
          return;
        }
        const imported = response.zone;
        if (!imported) {
          dispatch({
            type: "fail",
            errors: {
              ...noErrors,
              formError: "The server returned an empty zone response.",
            },
          });
          return;
        }
        // REPLACE regenerates every record id, so consumers drop the ids they hold.
        onImported(imported);
        onOpenChange(false);
        if (mode === "create") {
          router.push(zoneHref(imported.name));
        }
      },
      (error: unknown) => handleFailure(id, error),
    );
  }

  const title =
    mode === "create" ? "Import a zone file" : `Import into ${zone?.name ?? ""}`;
  const description =
    mode === "create"
      ? "Creates the zone from a BIND zone file. SOA and apex NS from the file are replaced by this service's."
      : "Replaces every record in this zone with the file's records. SOA and apex NS stay managed here.";
  const applyLabel = mode === "create" ? "Create zone" : "Replace zone";

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{description}</DialogDescription>
        </DialogHeader>

        {state.formError ? (
          <Alert variant="destructive">
            <AlertTitle>Import failed</AlertTitle>
            <AlertDescription>{state.formError}</AlertDescription>
          </Alert>
        ) : null}

        <div className="flex flex-col gap-4">
          {mode === "create" ? (
            <div className="grid gap-2">
              <Label htmlFor={domainInputId}>Domain</Label>
              <Input
                id={domainInputId}
                value={state.domain}
                onChange={(event) =>
                  dispatch({ type: "setDomain", value: event.target.value })
                }
                placeholder="studio.photos"
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                inputMode="url"
                disabled={busy}
                aria-invalid={
                  state.domainError || existingZone ? true : undefined
                }
                aria-describedby={domainMessageId}
              />
              <p
                id={domainMessageId}
                role={state.domainError || existingZone ? "alert" : undefined}
                className={
                  state.domainError || existingZone
                    ? "text-xs text-destructive"
                    : "text-xs text-muted-foreground"
                }
              >
                {existingZone ? (
                  <>
                    {`A zone called ${existingZone} already exists — `}
                    <Link
                      href={zoneHref(existingZone)}
                      className="link"
                    >
                      open it
                    </Link>
                    {" and use Import there."}
                  </>
                ) : (
                  (state.domainError ??
                  "The zone is created with this name; every record in the file must sit inside it.")
                )}
              </p>
            </div>
          ) : null}

          <div className="grid gap-2">
            <Label htmlFor={textId}>Zone file</Label>
            <Textarea
              id={textId}
              mono
              spellCheck={false}
              value={state.text}
              onChange={(event) =>
                dispatch({ type: "setText", value: event.target.value })
              }
              placeholder={"$ORIGIN example.com.\n@\t300\tIN\tA\t203.0.113.10"}
              className="h-56"
              disabled={busy}
              aria-invalid={fileError ? true : undefined}
              aria-describedby={textMessageId}
            />
            <p
              id={textMessageId}
              role={fileError ? "alert" : undefined}
              className={
                fileError
                  ? "text-xs text-destructive"
                  : "text-xs text-muted-foreground"
              }
            >
              {fileError ?? "Paste the records, or choose a file below (up to 1 MB)."}
            </p>
            <div className="flex flex-wrap items-center gap-2">
              <Label htmlFor={fileInputId} className="text-xs text-muted-foreground">
                Or choose a file
              </Label>
              <input
                id={fileInputId}
                type="file"
                accept=".zone,.txt,.db,text/plain"
                onChange={handleFile}
                disabled={busy}
                className="text-xs text-muted-foreground file:mr-2 file:rounded-md file:border file:border-input file:bg-transparent file:px-2 file:py-1 file:text-xs file:text-foreground"
              />
            </div>
          </div>

          {previewed && state.preview ? (
            <div className="flex flex-col gap-3 rounded-lg border border-line bg-fill p-4">
              <p className="eyebrow">Preview</p>
              <p className="text-ui">
                {`${state.preview.userRecords} ${plural(state.preview.userRecords, "record")} will be ${
                  mode === "create" ? "created in" : "written to"
                } ${zoneName}.`}
              </p>
              {state.preview.diff ? (
                <>
                  <p className="font-mono text-[13px] text-muted-foreground">
                    {`+${state.preview.diff.added} added · −${state.preview.diff.removed} removed · ${state.preview.diff.kept} unchanged`}
                  </p>
                  <p className="text-xs text-destructive">
                    {`This replaces every non-managed record in ${zoneName}.`}
                  </p>
                </>
              ) : null}
              {state.preview.warnings.length ? (
                <Alert role="status">
                  <AlertTitle>
                    {`${state.preview.warnings.length} ${plural(state.preview.warnings.length, "warning")}`}
                  </AlertTitle>
                  <AlertDescription>
                    <ul className="list-disc pl-4">
                      {state.preview.warnings.map((warning) => (
                        <li key={warning}>{warning}</li>
                      ))}
                    </ul>
                  </AlertDescription>
                </Alert>
              ) : null}
            </div>
          ) : null}
        </div>

        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={state.busy === "applying"}
          >
            Cancel
          </Button>
          <Button
            type="button"
            variant="outline"
            onClick={handlePreview}
            disabled={busy || previewed}
          >
            {state.busy === "previewing" ? "Checking…" : "Preview"}
          </Button>
          <Button
            type="button"
            variant={mode === "replace" ? "destructive" : "default"}
            onClick={handleApply}
            disabled={busy || !previewed}
          >
            {state.busy === "applying" ? "Importing…" : applyLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
