"use client";

import { useId, useState, type FormEvent } from "react";
import { Code } from "@connectrpc/connect";

import { useZones } from "@/components/app/zones-provider";
import { DismissGuardNote } from "@/components/app/dismiss-guard-note";
import { PlanPreview } from "@/components/guided/plan-preview";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import { fqdn, isPrintableTxt, toRelativeName } from "@/lib/dns-values";
import {
  applyPlanToZone,
  describePlan,
  planVerify,
  verifyNameError,
} from "@/lib/record-plan";
import { txtPresets } from "@/lib/service-presets";

export type VerifyServiceDialogProps = {
  open: boolean;
  zone: Zone;
  onOpenChange: (open: boolean) => void;
  onApplied?: (zone: Zone) => void;
};

const customId = "custom";
const alreadyPublished = "That record is already published.";

/** Drops a prefix the user pasted along with the token, whatever its case. */
function stripPrefix(value: string, prefix: string): string {
  const trimmed = value.trim();
  return trimmed.toLowerCase().startsWith(prefix.toLowerCase())
    ? trimmed.slice(prefix.length)
    : trimmed;
}

export function VerifyServiceDialog({
  open,
  zone,
  onOpenChange,
  onApplied,
}: VerifyServiceDialogProps) {
  const { createRecord, updateRecord, deleteRecord } = useZones();
  const ids = useId();
  const [presetId, setPresetId] = useState(txtPresets[0].id);
  const [token, setToken] = useState("");
  const [name, setName] = useState("@");
  const [submitting, setSubmitting] = useState(false);
  const [progress, setProgress] = useState<{ done: number; total: number }>();
  const [failure, setFailure] = useState<string>();
  const [duplicate, setDuplicate] = useState(false);

  const preset = txtPresets.find((entry) => entry.id === presetId);
  const custom = preset === undefined;
  const owner = custom ? name : "@";
  const text = preset
    ? `${preset.prefix}${stripPrefix(token, preset.prefix)}`
    : token.trim();

  const nameError = custom ? verifyNameError(zone, owner) : undefined;
  const valueError =
    token.trim() && !isPrintableTxt(text)
      ? "The value must be printable text (no control characters or line breaks)."
      : undefined;

  const plan = planVerify(zone, { name: owner, text });
  const lines = describePlan(plan);
  const alreadyThere = plan.notes.includes(alreadyPublished) || duplicate;
  const blocked =
    !token.trim() ||
    Boolean(nameError) ||
    Boolean(valueError) ||
    lines.length === 0;

  function reset() {
    setToken("");
    setFailure(undefined);
    setDuplicate(false);
    setProgress(undefined);
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (blocked || submitting) {
      return;
    }
    setSubmitting(true);
    setFailure(undefined);
    setDuplicate(false);
    setProgress({ done: 0, total: plan.ops.length });

    const { result, zone: written } = await applyPlanToZone(
      { createRecord, updateRecord, deleteRecord },
      zone.id,
      plan,
      (done, total) => setProgress({ done, total }),
    );

    setSubmitting(false);
    if (result.failed) {
      // The server has the last word on duplicates: another tab may have published the
      // same value between the plan being drawn and this write.
      if (result.failed.code === Code.AlreadyExists) {
        setDuplicate(true);
        return;
      }
      setFailure(result.failed.message);
      return;
    }
    reset();
    onOpenChange(false);
    onApplied?.(written ?? zone);
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        // Closing mid-write would unmount the form and drop the failure report for a
        // record the server may still refuse.
        if (!next && submitting) {
          return;
        }
        if (!next) {
          reset();
        }
        onOpenChange(next);
      }}
    >
      <DialogContent
        className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg"
        showCloseButton={!submitting}
        onEscapeKeyDown={(event) => {
          if (submitting) {
            event.preventDefault();
          }
        }}
        onInteractOutside={(event) => {
          if (submitting) {
            event.preventDefault();
          }
        }}
      >
        <form className="contents" onSubmit={handleSubmit}>
          <DialogHeader>
            <DialogTitle>Verify a service</DialogTitle>
            <DialogDescription>
              Paste the value a service gave you (Google, Microsoft, Stripe…). It
              becomes a TXT record at{" "}
              <span className="font-mono text-foreground">
                {fqdn(zone.name, toRelativeName(zone.name, owner))}
              </span>
              .
            </DialogDescription>
          </DialogHeader>

          <div className="flex flex-col gap-4 py-1">
            <div className="grid gap-2">
              <Label htmlFor={`${ids}-preset`}>Service</Label>
              <Select
                value={presetId}
                onValueChange={(next) => {
                  setPresetId(next);
                  reset();
                }}
                disabled={submitting}
              >
                <SelectTrigger id={`${ids}-preset`} className="h-9 w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {txtPresets.map((entry) => (
                    <SelectItem key={entry.id} value={entry.id}>
                      {entry.label}
                    </SelectItem>
                  ))}
                  <SelectItem value={customId}>Custom TXT</SelectItem>
                </SelectContent>
              </Select>
            </div>

            <div className="grid gap-2">
              <Label htmlFor={`${ids}-value`}>Verification value</Label>
              <div className="flex items-center gap-0">
                {preset ? (
                  <span className="rounded-l-lg border border-r-0 border-input px-2.5 py-[7px] font-mono text-[13px] text-muted-foreground">
                    {preset.prefix}
                  </span>
                ) : null}
                <Input
                  id={`${ids}-value`}
                  className={
                    preset ? "h-9 rounded-l-none font-mono" : "h-9 font-mono"
                  }
                  autoComplete="off"
                  spellCheck={false}
                  autoCapitalize="none"
                  autoFocus
                  placeholder={preset ? "9xk2…" : "my-service-verification=…"}
                  value={token}
                  onChange={(event) => {
                    setToken(event.target.value);
                    setFailure(undefined);
                    setDuplicate(false);
                  }}
                  aria-invalid={Boolean(valueError) || undefined}
                  aria-describedby={
                    valueError ? `${ids}-value-error` : `${ids}-value-help`
                  }
                  disabled={submitting}
                />
              </div>
              {valueError ? (
                <p
                  id={`${ids}-value-error`}
                  role="alert"
                  className="text-xs text-destructive"
                >
                  {valueError}
                </p>
              ) : (
                <p id={`${ids}-value-help`} className="text-xs text-muted-foreground">
                  Paste the token or the whole string — a repeated prefix is
                  removed for you.
                </p>
              )}
            </div>

            {custom ? (
              <div className="grid gap-2">
                <Label htmlFor={`${ids}-name`}>Name</Label>
                <Input
                  id={`${ids}-name`}
                  className="h-9 font-mono"
                  autoComplete="off"
                  spellCheck={false}
                  autoCapitalize="none"
                  placeholder="@"
                  value={name}
                  onChange={(event) => {
                    setName(event.target.value);
                    setFailure(undefined);
                    setDuplicate(false);
                  }}
                  aria-invalid={Boolean(nameError) || undefined}
                  aria-describedby={nameError ? `${ids}-name-error` : undefined}
                  disabled={submitting}
                />
                {nameError ? (
                  <p
                    id={`${ids}-name-error`}
                    role="alert"
                    className="text-xs text-destructive"
                  >
                    {nameError}
                  </p>
                ) : null}
              </div>
            ) : null}

            {alreadyThere ? (
              <p role="status" className="text-xs text-muted-foreground">
                {alreadyPublished}
              </p>
            ) : null}

            <PlanPreview
              lines={lines}
              notes={plan.notes.filter((note) => note !== alreadyPublished)}
              progress={submitting || failure ? progress : undefined}
              failure={failure}
            />

            <DismissGuardNote active={submitting} action="Writing the record" />
          </div>

          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={submitting}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={blocked || submitting}>
              {submitting ? "Publishing…" : "Publish record"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
