"use client";

import { useState } from "react";

import { RelativeTime } from "@/components/app/relative-time";
import { useSiteEnvVars } from "@/components/hosting/use-site-env-vars";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import type { EnvVar } from "@/gen/hosting/v1/hosting_pb";
import { describeError } from "@/lib/errors";
import { toDate } from "@/lib/format";
import {
  defaultEnvEnvironment,
  envEnvironmentLabel,
  envEnvironments,
  groupEnvVars,
} from "@/lib/platform-model";
import { getHostingClient } from "@/lib/platform-client";

export type SiteEnvPanelProps = {
  zoneId: string;
  zoneName: string;
  /** Rendered above the list; the sheet puts its own copy in the header instead. */
  intro?: boolean;
};

/** The facade's own rule, applied here so a refusal costs no re-entry of the value. */
const namePattern = /^[A-Za-z_][A-Za-z0-9_]*$/;
const maxNameLength = 128;

/**
 * The environment variables of one site: what is set, in which environment, and
 * when it was last written — never a value.
 *
 * Values are write-only end to end. `EnvVar` has no value field, so nothing a
 * variable holds is ever read back and an "edit" is a re-entry of the whole
 * value; the input below is a password field with autofill off, it is cleared
 * the moment the request settles, and the value is never put in a URL, in this
 * component's state after the write, or in any log.
 *
 * A change does not reach the running site until it is built again. Both
 * mutating RPCs return that sentence and it is shown verbatim.
 */
export function SiteEnvPanel({ zoneId, zoneName, intro }: SiteEnvPanelProps) {
  const env = useSiteEnvVars(true, zoneId);
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [environment, setEnvironment] = useState<string>(
    defaultEnvEnvironment,
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [note, setNote] = useState<string>();
  const [confirming, setConfirming] = useState<string>();

  const trimmedName = name.trim();
  const nameInvalid =
    trimmedName.length > 0 &&
    (!namePattern.test(trimmedName) || trimmedName.length > maxNameLength);
  const canSubmit = trimmedName.length > 0 && !nameInvalid && !busy;

  async function handleSet() {
    setError(undefined);
    setNote(undefined);
    setBusy(true);
    try {
      const response = await getHostingClient().setSiteEnvVar({
        zoneId,
        name: trimmedName,
        value,
        environment,
      });
      setName("");
      setNote(response.note);
      await env.refresh();
    } catch (caught) {
      setError(describeError(caught));
    } finally {
      // The value leaves this component with the request and is not kept, not
      // even to retry: it was never readable back, so there is nothing to keep.
      setValue("");
      setBusy(false);
    }
  }

  async function handleDelete(variable: EnvVar) {
    setError(undefined);
    setNote(undefined);
    setBusy(true);
    try {
      const response = await getHostingClient().deleteSiteEnvVar({
        zoneId,
        name: variable.name,
        environment: variable.environment,
      });
      setConfirming(undefined);
      setNote(response.note);
      await env.refresh();
    } catch (caught) {
      setError(describeError(caught));
    } finally {
      setBusy(false);
    }
  }

  if (!env.loaded) {
    return (
      <div className="flex flex-col gap-2">
        <Skeleton className="h-4 w-1/3" />
        <Skeleton className="h-4 w-2/3" />
        <Skeleton className="h-4 w-1/2" />
      </div>
    );
  }

  // The engine could not be asked at all: the list is empty because there was no
  // RPC to ask with, which is not the same statement as "this site has none".
  if (!env.supported) {
    return (
      <div className="flex flex-col gap-2">
        <p className="text-ui rounded-[10px] border border-line bg-fill-faint px-3.5 py-2.5 text-warning">
          {env.note ||
            "This hosting engine version cannot store environment variables."}
        </p>
        {env.error ? (
          <p role="alert" className="text-xs text-destructive">
            {env.error}
          </p>
        ) : null}
      </div>
    );
  }

  // The list never landed. `variables` is empty because nothing answered, which is
  // not the statement "this site has none" — so no per-environment "None set." line
  // may be printed here, and the add form is withheld too: a write against an engine
  // that is not answering would only add a second failure.
  if (env.error && env.variables.length === 0) {
    return (
      <div className="flex flex-col gap-2">
        <p
          role="alert"
          className="text-ui rounded-[10px] border border-line bg-fill-faint px-3.5 py-2.5 text-warning"
        >
          The hosting engine didn&apos;t answer, so the variables it holds for{" "}
          {zoneName} can&apos;t be listed: {env.error}
        </p>
        <Button
          size="sm"
          variant="outline"
          className="self-start"
          onClick={() => {
            void env.refresh();
          }}
        >
          Retry
        </Button>
      </div>
    );
  }

  const groups = groupEnvVars(env.variables);

  return (
    <div className="flex flex-col gap-4">
      {intro ? (
        <p className="text-subtle leading-[1.55]">
          Values are sent straight to the hosting engine and never shown again —
          changing one means entering the whole value.
        </p>
      ) : null}

      <div className="flex flex-col gap-3.5">
        {groups.map((group) => (
          <div key={group.environment} className="flex flex-col gap-1.5">
            <p className="eyebrow">{envEnvironmentLabel(group.environment)}</p>
            {group.variables.length === 0 ? (
              <p className="text-ui text-muted-foreground">None set.</p>
            ) : (
              <ul className="flex flex-col gap-1">
                {group.variables.map((variable) => {
                  const key = `${group.environment}:${variable.name}`;
                  const lastSet = toDate(variable.lastSet);
                  return (
                    <li
                      key={key}
                      className="text-ui flex flex-wrap items-center gap-x-3 gap-y-1 rounded-[10px] border border-line px-3 py-2"
                    >
                      <span className="font-mono break-all">
                        {variable.name}
                      </span>
                      <span className="text-xs text-muted-foreground">
                        {lastSet ? (
                          <>
                            set <RelativeTime date={lastSet} />
                          </>
                        ) : (
                          "set time not reported"
                        )}
                      </span>
                      {confirming === key ? (
                        <span className="ml-auto flex items-center gap-2">
                          <span className="text-xs text-warning">
                            Remove it?
                          </span>
                          <Button
                            size="sm"
                            variant="destructive"
                            disabled={busy}
                            onClick={() => {
                              void handleDelete(variable);
                            }}
                          >
                            Remove
                          </Button>
                          <Button
                            size="sm"
                            variant="outline"
                            disabled={busy}
                            onClick={() => setConfirming(undefined)}
                          >
                            Cancel
                          </Button>
                        </span>
                      ) : (
                        <Button
                          size="sm"
                          variant="outline"
                          className="ml-auto"
                          disabled={busy}
                          aria-label={`Remove ${variable.name} from ${group.environment}`}
                          onClick={() => setConfirming(key)}
                        >
                          Remove
                        </Button>
                      )}
                    </li>
                  );
                })}
              </ul>
            )}
          </div>
        ))}
      </div>

      <form
        autoComplete="off"
        className="flex flex-col gap-3 rounded-[10px] border border-line bg-fill-faint px-3.5 py-3"
        onSubmit={(event) => {
          event.preventDefault();
          if (canSubmit) {
            void handleSet();
          }
        }}
      >
        <p className="eyebrow">Add or replace a variable</p>
        <div className="grid gap-3 sm:grid-cols-2">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor={`env-name-${zoneId}`}>Name</Label>
            <Input
              id={`env-name-${zoneId}`}
              value={name}
              onChange={(event) => setName(event.target.value)}
              placeholder="API_URL"
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              disabled={busy}
              aria-invalid={nameInvalid || undefined}
              className="font-mono text-[13px]"
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor={`env-environment-${zoneId}`}>Environment</Label>
            <Select
              value={environment}
              onValueChange={setEnvironment}
              disabled={busy}
            >
              <SelectTrigger
                id={`env-environment-${zoneId}`}
                className="w-full"
              >
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {envEnvironments.map((option) => (
                  <SelectItem key={option} value={option}>
                    {envEnvironmentLabel(option)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`env-value-${zoneId}`}>Value</Label>
          <Input
            id={`env-value-${zoneId}`}
            type="password"
            value={value}
            onChange={(event) => setValue(event.target.value)}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            disabled={busy}
            className="font-mono text-[13px]"
          />
          <p className="text-xs text-muted-foreground">
            Write-only: nothing reads a value back, so this box is empty again
            as soon as it is sent.
          </p>
        </div>
        {nameInvalid ? (
          <p role="alert" className="text-xs text-destructive">
            Use letters, digits and underscores, starting with a letter or an
            underscore.
          </p>
        ) : null}
        <div className="flex flex-wrap items-center gap-3">
          <Button type="submit" size="sm" disabled={!canSubmit}>
            {busy ? "Saving…" : "Save variable"}
          </Button>
          <span className="text-xs text-muted-foreground">
            {zoneName} keeps the values it was last built with — deploy again
            for a change here to take effect.
          </span>
        </div>
      </form>

      {note ? (
        <p role="status" className="text-xs text-muted-foreground">
          {note}
        </p>
      ) : null}
      {error ? (
        <p role="alert" className="text-xs text-destructive">
          {error}
        </p>
      ) : null}
      {env.error ? (
        <p role="alert" className="text-xs text-destructive">
          {env.error}
        </p>
      ) : null}
    </div>
  );
}
