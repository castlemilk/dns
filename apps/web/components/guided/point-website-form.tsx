"use client";

import { useId, useState, type FormEvent, type ReactNode } from "react";

import { PlanPreview } from "@/components/guided/plan-preview";
import { useZones } from "@/components/app/zones-provider";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { RecordType, type Zone } from "@/gen/dns/v1/dns_pb";
import {
  isHostname,
  isIPv4,
  looksIPv6,
  normaliseTarget,
  sameHost,
} from "@/lib/dns-values";
import {
  applyPlanToZone,
  describePlan,
  planWebsite,
  splitFieldError,
  validateWebsiteInput,
  type WebsiteInput,
} from "@/lib/record-plan";
import { cn } from "@/lib/utils";

export type PointWebsiteFormProps = {
  zone: Zone;
  layout: "inline" | "dialog";
  submitLabel: string;
  onApplied: (zone: Zone) => void;
  onCancel?: () => void;
  /** Lets the connect flow host the submit button in its own footer row. */
  footerSlot?: (submit: ReactNode) => ReactNode;
  /**
   * Told when a plan starts and stops being written, so a dialog host can refuse to
   * close mid-apply (unmounting the form would drop the progress and failure report).
   */
  onSubmittingChange?: (submitting: boolean) => void;
};

type Field = "ipv4" | "ipv6" | "target";

function fieldForRecordType(type: RecordType | undefined): Field | undefined {
  switch (type) {
    case RecordType.A:
      return "ipv4";
    case RecordType.AAAA:
      return "ipv6";
    case RecordType.CNAME:
      return "target";
    default:
      return undefined;
  }
}

export function PointWebsiteForm({
  zone,
  layout,
  submitLabel,
  onApplied,
  onCancel,
  footerSlot,
  onSubmittingChange,
}: PointWebsiteFormProps) {
  const { createRecord, updateRecord, deleteRecord } = useZones();
  const ids = useId();
  const [mode, setMode] = useState<"ip" | "host">("ip");
  const [ipv4, setIpv4] = useState("");
  const [ipv6, setIpv6] = useState("");
  const [target, setTarget] = useState("");
  const [www, setWww] = useState(true);
  const [showAddresses, setShowAddresses] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [progress, setProgress] = useState<{ done: number; total: number }>();
  const [failure, setFailure] = useState<string>();
  const [fieldError, setFieldError] = useState<{
    field: Field;
    message: string;
  }>();

  const input: WebsiteInput = {
    mode,
    ipv4: mode === "ip" || showAddresses ? ipv4 : "",
    ipv6: mode === "ip" || showAddresses ? ipv6 : "",
    target,
    www,
  };
  const plan = planWebsite(zone, input);
  const lines = describePlan(plan);

  const shapeError = (() => {
    if (input.ipv4?.trim() && !isIPv4(ipv4)) {
      return { field: "ipv4" as const, message: "Enter an IPv4 address such as 203.0.113.10." };
    }
    if (input.ipv6?.trim() && !looksIPv6(ipv6)) {
      return { field: "ipv6" as const, message: "Enter an IPv6 address such as 2001:db8::10." };
    }
    if (mode === "host" && target.trim() && !isHostname(target)) {
      return { field: "target" as const, message: "Enter a host name such as your-site.example-host.net." };
    }
    // `CNAME www → www.<zone>` is a record the server accepts and no resolver can
    // follow: the alias would point at itself.
    if (
      mode === "host" &&
      target.trim() &&
      sameHost(normaliseTarget(zone.name, target.trim()), `www.${zone.name}`)
    ) {
      return {
        field: "target" as const,
        message: `Enter the host name your provider gave you, not www.${zone.name} itself.`,
      };
    }
    return undefined;
  })();
  const missing = validateWebsiteInput(input);
  const shown = fieldError ?? shapeError;
  const blocked = Boolean(shapeError) || Boolean(missing) || lines.length === 0;

  function clear(field: Field) {
    setFieldError((current) => (current?.field === field ? undefined : current));
    setFailure(undefined);
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (blocked || submitting) {
      return;
    }
    setSubmitting(true);
    onSubmittingChange?.(true);
    setFailure(undefined);
    setFieldError(undefined);
    setProgress({ done: 0, total: plan.ops.length });

    const { result, zone: written } = await applyPlanToZone(
      { createRecord, updateRecord, deleteRecord },
      zone.id,
      plan,
      (done, total) => setProgress({ done, total }),
    );

    setSubmitting(false);
    onSubmittingChange?.(false);
    if (result.failed) {
      const split = splitFieldError(result.failed.message);
      const failed = result.failed.op;
      const field =
        split.field === "value" && failed.op !== "delete"
          ? fieldForRecordType(failed.draft.type)
          : undefined;
      if (field) {
        setFieldError({ field, message: split.message });
      }
      setFailure(result.failed.message);
      return;
    }
    onApplied(written ?? zone);
  }

  const errorId = `${ids}-error`;
  const describe = (field: Field) =>
    shown?.field === field ? errorId : undefined;

  const fieldMessage = (field: Field) =>
    shown?.field === field ? (
      <p id={errorId} role="alert" className="text-xs text-destructive">
        {shown.message}
      </p>
    ) : null;

  const submit = (
    <Button
      type="submit"
      size="lg"
      disabled={blocked || submitting}
      className={cn(
        "rounded-[7px] px-4 font-semibold",
        layout === "inline" && "text-ui h-10",
      )}
    >
      {submitting ? "Writing records…" : submitLabel}
    </Button>
  );

  return (
    <form onSubmit={handleSubmit} className="flex flex-col gap-5">
      <fieldset className="flex flex-col gap-2.5" disabled={submitting}>
        <legend className="text-ui mb-2.5 font-medium">
          How does your host identify the site?
        </legend>
        <RadioGroup
          value={mode}
          onValueChange={(next) => {
            setMode(next as "ip" | "host");
            setFieldError(undefined);
            setFailure(undefined);
          }}
          aria-label="How does your host identify the site?"
          className="gap-2.5"
        >
          <div className="flex items-center gap-2.5">
            <RadioGroupItem value="ip" id={`${ids}-mode-ip`} />
            <Label htmlFor={`${ids}-mode-ip`} className="text-ui font-normal">
              IP addresses
            </Label>
          </div>
          <div className="flex items-center gap-2.5">
            <RadioGroupItem value="host" id={`${ids}-mode-host`} />
            <Label htmlFor={`${ids}-mode-host`} className="text-ui font-normal">
              Host name for www (CNAME)
            </Label>
          </div>
        </RadioGroup>
      </fieldset>

      {mode === "ip" ? (
        <div className="flex flex-col gap-4">
          <div className="grid gap-2">
            <Label htmlFor={`${ids}-ipv4`}>IPv4 address for {zone.name}</Label>
            <Input
              id={`${ids}-ipv4`}
              className="h-9 font-mono"
              autoComplete="off"
              spellCheck={false}
              inputMode="decimal"
              placeholder="203.0.113.10"
              value={ipv4}
              onChange={(event) => {
                setIpv4(event.target.value);
                clear("ipv4");
              }}
              aria-invalid={shown?.field === "ipv4" || undefined}
              aria-describedby={describe("ipv4")}
              disabled={submitting}
            />
            {fieldMessage("ipv4")}
          </div>
          <div className="grid gap-2">
            <Label htmlFor={`${ids}-ipv6`}>IPv6 address (optional)</Label>
            <Input
              id={`${ids}-ipv6`}
              className="h-9 font-mono"
              autoComplete="off"
              spellCheck={false}
              placeholder="2001:db8::10"
              value={ipv6}
              onChange={(event) => {
                setIpv6(event.target.value);
                clear("ipv6");
              }}
              aria-invalid={shown?.field === "ipv6" || undefined}
              aria-describedby={describe("ipv6")}
              disabled={submitting}
            />
            {fieldMessage("ipv6")}
          </div>
          <div className="flex items-center gap-2.5">
            <Checkbox
              id={`${ids}-www`}
              checked={www}
              onCheckedChange={(checked) => setWww(checked === true)}
              disabled={submitting}
            />
            <Label htmlFor={`${ids}-www`} className="text-ui font-normal">
              Also point www.{zone.name} here
            </Label>
          </div>
        </div>
      ) : (
        <div className="flex flex-col gap-4">
          <div className="grid gap-2">
            <Label htmlFor={`${ids}-target`}>Host name from your provider</Label>
            <Input
              id={`${ids}-target`}
              className="h-9 font-mono"
              autoComplete="off"
              spellCheck={false}
              autoCapitalize="none"
              placeholder="your-site.example-host.net"
              value={target}
              onChange={(event) => {
                setTarget(event.target.value);
                clear("target");
              }}
              aria-invalid={shown?.field === "target" || undefined}
              aria-describedby={describe("target") ?? `${ids}-target-help`}
              disabled={submitting}
            />
            {fieldMessage("target") ?? (
              <p id={`${ids}-target-help`} className="text-xs text-muted-foreground">
                The apex can&rsquo;t be an alias — {zone.name} itself keeps its
                current records. Add the host&rsquo;s IP under IP addresses if
                they gave you one.
              </p>
            )}
          </div>

          <div className="flex flex-col gap-3">
            <button
              type="button"
              onClick={() => setShowAddresses((open) => !open)}
              aria-expanded={showAddresses}
              aria-controls={`${ids}-addresses`}
              className="link self-start rounded-sm text-[13px] outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              {showAddresses
                ? `Hide the address fields for ${zone.name}`
                : `Also set an address for ${zone.name}`}
            </button>
            <div
              id={`${ids}-addresses`}
              hidden={!showAddresses}
              className="flex flex-col gap-4"
            >
              <div className="grid gap-2">
                <Label htmlFor={`${ids}-host-ipv4`}>IPv4 address (optional)</Label>
                <Input
                  id={`${ids}-host-ipv4`}
                  className="h-9 font-mono"
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="203.0.113.10"
                  value={ipv4}
                  onChange={(event) => {
                    setIpv4(event.target.value);
                    clear("ipv4");
                  }}
                  aria-invalid={shown?.field === "ipv4" || undefined}
                  aria-describedby={describe("ipv4")}
                  disabled={submitting}
                />
                {fieldMessage("ipv4")}
              </div>
              <div className="grid gap-2">
                <Label htmlFor={`${ids}-host-ipv6`}>IPv6 address (optional)</Label>
                <Input
                  id={`${ids}-host-ipv6`}
                  className="h-9 font-mono"
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="2001:db8::10"
                  value={ipv6}
                  onChange={(event) => {
                    setIpv6(event.target.value);
                    clear("ipv6");
                  }}
                  aria-invalid={shown?.field === "ipv6" || undefined}
                  aria-describedby={describe("ipv6")}
                  disabled={submitting}
                />
                {fieldMessage("ipv6")}
              </div>
            </div>
          </div>
        </div>
      )}

      <p className="text-xs text-muted-foreground">
        TTL 300 s (or the TTL already used by the same records).
      </p>

      {missing && !shapeError ? (
        <p role="alert" className="text-xs text-warning">
          {missing}
        </p>
      ) : null}

      <PlanPreview
        lines={lines}
        notes={plan.notes}
        progress={submitting || failure ? progress : undefined}
        failure={failure}
      />

      {footerSlot ? (
        footerSlot(submit)
      ) : (
        <div className="flex flex-wrap items-center justify-end gap-2.5">
          {onCancel ? (
            <Button
              type="button"
              variant="outline"
              onClick={onCancel}
              disabled={submitting}
            >
              Cancel
            </Button>
          ) : null}
          {submit}
        </div>
      )}
    </form>
  );
}
