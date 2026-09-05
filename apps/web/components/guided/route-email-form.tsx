"use client";

import { useId, useState, type FormEvent, type ReactNode } from "react";
import { Plus, X } from "lucide-react";

import { useZones } from "@/components/app/zones-provider";
import { PlanPreview } from "@/components/guided/plan-preview";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { RecordType, type Zone } from "@/gen/dns/v1/dns_pb";
import {
  isEmailAddress,
  applyPlanToZone,
  describePlan,
  mxValue,
  planEmail,
  type EmailInput,
} from "@/lib/record-plan";
import {
  defaultMailPresetId,
  mailPreset,
  mailPresets,
  mailPresetToken,
  mailPresetTokenProblem,
  presetSpfValue,
  type MailPreset,
} from "@/lib/email-presets";
import {
  isHostname,
  isPrintableTxt,
  isSpfText,
  lower,
  parseDmarc,
  unquoteTxt,
} from "@/lib/dns-values";
import { cn } from "@/lib/utils";

export type RouteEmailFormProps = {
  zone: Zone;
  layout: "inline" | "dialog";
  submitLabel: string;
  onApplied: (zone: Zone) => void;
  onCancel?: () => void;
  footerSlot?: (submit: ReactNode) => ReactNode;
  /**
   * Told when a plan starts and stops being written, so a dialog host can refuse to
   * close mid-apply (unmounting the form would drop the progress and failure report).
   */
  onSubmittingChange?: (submitting: boolean) => void;
};

type MxRow = { key: string; priority: string; host: string };
type DkimRow = { key: string; selector: string; target: string };

let rowSeq = 0;
const nextKey = () => {
  rowSeq += 1;
  return `row-${rowSeq}`;
};

const dmarcPolicies = ["none", "quarantine", "reject"] as const;
type DmarcPolicy = (typeof dmarcPolicies)[number];

function presetRows(preset: MailPreset): MxRow[] {
  return preset.mx.map((entry) => ({
    key: nextKey(),
    priority: String(entry.priority),
    host: entry.host,
  }));
}

function presetDkim(preset: MailPreset, zoneName: string): DkimRow[] {
  return (preset.dkim?.(zoneName) ?? []).map((entry) => ({
    key: nextKey(),
    selector: entry.selector,
    target: entry.target,
  }));
}

export function RouteEmailForm({
  zone,
  layout,
  submitLabel,
  onApplied,
  onCancel,
  footerSlot,
  onSubmittingChange,
}: RouteEmailFormProps) {
  const { createRecord, updateRecord, deleteRecord } = useZones();
  const ids = useId();

  const existingSpf = zone.records.find(
    (record) =>
      !record.managed &&
      lower(record.name) === "@" &&
      record.type === RecordType.TXT &&
      isSpfText(unquoteTxt(record.value)),
  );
  const existingDmarc = zone.records.find(
    (record) =>
      !record.managed &&
      lower(record.name) === "_dmarc" &&
      record.type === RecordType.TXT,
  );
  const dmarcParsed = existingDmarc
    ? parseDmarc(unquoteTxt(existingDmarc.value))
    : undefined;
  const dmarcLocked = dmarcParsed?.valid === true;

  const initialPreset = mailPreset(defaultMailPresetId);
  const [presetId, setPresetId] = useState(defaultMailPresetId);
  const [token, setToken] = useState("");
  const [rows, setRows] = useState<MxRow[]>(() => presetRows(initialPreset));
  const [spfOn, setSpfOn] = useState(true);
  const [spfValue, setSpfValue] = useState(() => presetSpfValue(initialPreset));
  const [spfMode, setSpfMode] = useState<"merge" | "replace">("merge");
  const [dmarcOn, setDmarcOn] = useState(true);
  const [policy, setPolicy] = useState<DmarcPolicy>("none");
  const [rua, setRua] = useState(`dmarc@${zone.name}`);
  const [dkimRows, setDkimRows] = useState<DkimRow[]>(() =>
    presetDkim(initialPreset, zone.name),
  );
  const [submitting, setSubmitting] = useState(false);
  const [progress, setProgress] = useState<{ done: number; total: number }>();
  const [failure, setFailure] = useState<string>();

  const preset = mailPreset(presetId);
  const isNone = preset.id === "none";
  const effectiveSpfMode: "merge" | "replace" = preset.spfInclude
    ? spfMode
    : "replace";
  // "Merge" only locks the field when there is a record to merge into; with no SPF
  // published, planEmail writes this value verbatim, so it stays editable (§G.5).
  const spfEditable = effectiveSpfMode === "replace" || !existingSpf;

  function choosePreset(id: string) {
    const next = mailPreset(id);
    setPresetId(id);
    setToken("");
    setRows(presetRows(next));
    setSpfOn(true);
    setSpfValue(presetSpfValue(next));
    setSpfMode(next.spfInclude ? "merge" : "replace");
    setPolicy(next.id === "none" ? "reject" : "none");
    setDkimRows(presetDkim(next, zone.name));
    setFailure(undefined);
  }

  function changeToken(value: string) {
    setToken(value);
    setFailure(undefined);
    if (!preset.needsToken) {
      return;
    }
    // The token is normalised before it is applied, so pasting the whole MX host the
    // provider displays builds the host once, not twice.
    const bare = mailPresetToken(preset, value);
    setRows(
      bare
        ? preset.needsToken.apply(bare).map((entry) => ({
            key: nextKey(),
            priority: String(entry.priority),
            host: entry.host,
          }))
        : [],
    );
  }

  const mx = rows
    .map((row) => ({ priority: Number(row.priority), host: row.host.trim() }))
    .filter(
      (entry) => entry.host.length > 0 && Number.isInteger(entry.priority),
    );
  const input: EmailInput = {
    mx,
    spf: spfOn
      ? {
          mode: effectiveSpfMode,
          include: preset.spfInclude,
          value: spfValue,
        }
      : undefined,
    dmarc:
      !dmarcLocked && dmarcOn ? { policy, rua: rua.trim() } : undefined,
    dkim: dkimRows
      .filter((row) => row.selector.trim() && row.target.trim())
      .map((row) => ({ selector: row.selector.trim(), target: row.target.trim() })),
  };
  const plan = planEmail(zone, input);
  const lines = describePlan(plan);

  const problem = (() => {
    if (preset.needsToken && !mailPresetToken(preset, token)) {
      return "Enter the MX token your provider gave you.";
    }
    const tokenProblem = mailPresetTokenProblem(preset, token);
    if (tokenProblem) {
      return tokenProblem;
    }
    if (mx.length === 0) {
      return "Add at least one mail server.";
    }
    if (
      !isNone &&
      mx.some((entry) => !isHostname(entry.host))
    ) {
      return "Every mail server needs a host name such as mx1.example.net.";
    }
    if (mx.some((entry) => entry.priority < 0 || entry.priority > 65535)) {
      return "Priorities must be between 0 and 65535.";
    }
    // One MX record cannot be written twice: the second create fails with "already
    // exists" after the first has been applied.
    const mxValues = mx.map((entry) =>
      mxValue(zone.name, entry.priority, entry.host),
    );
    if (new Set(mxValues).size !== mxValues.length) {
      return "Two rows name the same mail server.";
    }
    if (spfOn && spfEditable && !isSpfText(spfValue)) {
      return "The SPF record must start with v=spf1.";
    }
    if (spfOn && !isPrintableTxt(spfValue)) {
      return "The SPF record must be printable text (no control characters or line breaks).";
    }
    if (!dmarcLocked && dmarcOn && rua.trim() && !isEmailAddress(rua)) {
      return "Enter an email address for the DMARC reports, or clear the field.";
    }
    if (
      input.dkim.some(
        (entry) => !isHostname(entry.target) || !/^[a-z0-9_-]+$/i.test(entry.selector),
      )
    ) {
      return "Each DKIM row needs a selector and a host name.";
    }
    // Two rows on one selector would be two CNAMEs at the same name, which the server
    // refuses once the first is written.
    const selectors = input.dkim.map((entry) => lower(entry.selector));
    if (new Set(selectors).size !== selectors.length) {
      return "Each DKIM row needs a different selector.";
    }
    return undefined;
  })();

  const blocked = Boolean(problem) || lines.length === 0;

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (blocked || submitting) {
      return;
    }
    setSubmitting(true);
    onSubmittingChange?.(true);
    setFailure(undefined);
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
      setFailure(result.failed.message);
      return;
    }
    onApplied(written ?? zone);
  }

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
      <div className="grid gap-2">
        <Label htmlFor={`${ids}-provider`}>Mail provider</Label>
        <Select
          value={presetId}
          onValueChange={choosePreset}
          disabled={submitting}
        >
          <SelectTrigger id={`${ids}-provider`} className="h-9 w-full">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {mailPresets.map((entry) => (
              <SelectItem key={entry.id} value={entry.id}>
                {entry.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <p className="text-xs text-muted-foreground">
          {preset.hint}
          {/* "Custom" and "No mail for this domain" are not providers, so the
              sentence about a provider's documentation would name a vendor that
              does not exist. */}
          {preset.id === "custom" || isNone ? null : (
            <>
              {" "}
              Defaults from {preset.label}&rsquo;s documentation — check them
              against your provider&rsquo;s setup page.
            </>
          )}
        </p>
      </div>

      {preset.needsToken ? (
        <div className="grid gap-2">
          <Label htmlFor={`${ids}-token`}>{preset.needsToken.label}</Label>
          <Input
            id={`${ids}-token`}
            className="h-9 font-mono"
            autoComplete="off"
            spellCheck={false}
            placeholder={preset.needsToken.placeholder}
            value={token}
            onChange={(event) => changeToken(event.target.value)}
            disabled={submitting}
          />
        </div>
      ) : null}

      <fieldset className="flex flex-col gap-2.5" disabled={submitting}>
        <legend className="text-ui mb-2.5 font-medium">Mail servers</legend>
        {isNone ? (
          <p className="text-ui rounded-lg bg-fill px-3 py-2.5 font-mono text-[13px] text-soft">
            MX @ 0 .{" "}
            <span className="font-sans text-xs text-muted-foreground">
              — null MX (RFC 7505): rejects all mail
            </span>
          </p>
        ) : (
          <>
            <ul className="flex flex-col gap-2">
              {rows.map((row, index) => (
                <li key={row.key} className="flex items-end gap-2">
                  <div className="grid w-20 shrink-0 gap-1.5">
                    <Label
                      htmlFor={`${ids}-mx-priority-${row.key}`}
                      className="text-xs text-muted-foreground"
                    >
                      Priority
                    </Label>
                    <Input
                      id={`${ids}-mx-priority-${row.key}`}
                      type="number"
                      min={0}
                      max={65535}
                      inputMode="numeric"
                      className="h-9 font-mono tabular-nums"
                      value={row.priority}
                      onChange={(event) => {
                        const value = event.target.value;
                        setRows((current) =>
                          current.map((entry) =>
                            entry.key === row.key
                              ? { ...entry, priority: value }
                              : entry,
                          ),
                        );
                        setFailure(undefined);
                      }}
                    />
                  </div>
                  <div className="grid min-w-0 flex-1 gap-1.5">
                    <Label
                      htmlFor={`${ids}-mx-host-${row.key}`}
                      className="text-xs text-muted-foreground"
                    >
                      Mail server {index + 1}
                    </Label>
                    <Input
                      id={`${ids}-mx-host-${row.key}`}
                      className="h-9 font-mono"
                      autoComplete="off"
                      spellCheck={false}
                      autoCapitalize="none"
                      placeholder="mx1.example.net"
                      value={row.host}
                      onChange={(event) => {
                        const value = event.target.value;
                        setRows((current) =>
                          current.map((entry) =>
                            entry.key === row.key
                              ? { ...entry, host: value }
                              : entry,
                          ),
                        );
                        setFailure(undefined);
                      }}
                    />
                  </div>
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon-lg"
                    aria-label={`Remove mail server ${index + 1}`}
                    onClick={() =>
                      setRows((current) =>
                        current.filter((entry) => entry.key !== row.key),
                      )
                    }
                  >
                    <X aria-hidden="true" />
                  </Button>
                </li>
              ))}
            </ul>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="self-start"
              onClick={() =>
                setRows((current) => [
                  ...current,
                  { key: nextKey(), priority: "10", host: "" },
                ])
              }
            >
              <Plus aria-hidden="true" />
              Add server
            </Button>
          </>
        )}
      </fieldset>

      <div className="flex flex-col gap-2.5">
        <div className="flex items-center gap-2.5">
          <Checkbox
            id={`${ids}-spf`}
            checked={spfOn}
            onCheckedChange={(checked) => setSpfOn(checked === true)}
            disabled={submitting}
          />
          <Label htmlFor={`${ids}-spf`} className="text-ui font-normal">
            Write SPF
          </Label>
        </div>
        {spfOn ? (
          <div className="flex flex-col gap-2.5">
            {existingSpf && preset.spfInclude ? (
              <RadioGroup
                value={spfMode}
                onValueChange={(next) => setSpfMode(next as "merge" | "replace")}
                className="gap-2"
                aria-label="What to do with the existing SPF record"
              >
                <div className="flex items-center gap-2.5">
                  <RadioGroupItem value="merge" id={`${ids}-spf-merge`} />
                  <Label
                    htmlFor={`${ids}-spf-merge`}
                    className="text-ui font-normal"
                  >
                    Add the include to the existing record
                  </Label>
                </div>
                <div className="flex items-center gap-2.5">
                  <RadioGroupItem value="replace" id={`${ids}-spf-replace`} />
                  <Label
                    htmlFor={`${ids}-spf-replace`}
                    className="text-ui font-normal"
                  >
                    Replace it with this value
                  </Label>
                </div>
              </RadioGroup>
            ) : null}
            <div className="grid gap-2">
              <Label htmlFor={`${ids}-spf-value`}>SPF record</Label>
              <Input
                id={`${ids}-spf-value`}
                className="h-9 font-mono"
                autoComplete="off"
                spellCheck={false}
                value={spfValue}
                onChange={(event) => {
                  setSpfValue(event.target.value);
                  setFailure(undefined);
                }}
                disabled={submitting || !spfEditable}
              />
              {!spfEditable ? (
                <p className="text-xs text-muted-foreground">
                  The include is added to the record already published; the
                  preview shows the result.
                </p>
              ) : null}
            </div>
          </div>
        ) : null}
      </div>

      <div className="flex flex-col gap-2.5">
        {dmarcLocked ? (
          <p className="text-ui text-muted-foreground">
            DMARC — already set: p={dmarcParsed?.policy}, left unchanged.
          </p>
        ) : (
          <>
            <div className="flex items-center gap-2.5">
              <Checkbox
                id={`${ids}-dmarc`}
                checked={dmarcOn}
                onCheckedChange={(checked) => setDmarcOn(checked === true)}
                disabled={submitting}
              />
              <Label htmlFor={`${ids}-dmarc`} className="text-ui font-normal">
                Write DMARC
              </Label>
            </div>
            {dmarcOn ? (
              <div className="grid gap-4 sm:grid-cols-[10rem_minmax(0,1fr)]">
                <div className="grid gap-2">
                  <Label htmlFor={`${ids}-policy`}>Policy</Label>
                  <Select
                    value={policy}
                    onValueChange={(next) => setPolicy(next as DmarcPolicy)}
                    disabled={submitting}
                  >
                    <SelectTrigger id={`${ids}-policy`} className="h-9 w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {dmarcPolicies.map((entry) => (
                        <SelectItem key={entry} value={entry}>
                          p={entry}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                <div className="grid gap-2">
                  <Label htmlFor={`${ids}-rua`}>Report address</Label>
                  <Input
                    id={`${ids}-rua`}
                    className="h-9 font-mono"
                    autoComplete="off"
                    spellCheck={false}
                    inputMode="email"
                    value={rua}
                    onChange={(event) => {
                      setRua(event.target.value);
                      setFailure(undefined);
                    }}
                    disabled={submitting}
                  />
                </div>
              </div>
            ) : null}
          </>
        )}
      </div>

      <div className="flex flex-col gap-2.5">
        <p className="text-ui font-medium">DKIM</p>
        {dkimRows.length > 0 ? (
          <ul className="flex flex-col gap-2">
            {dkimRows.map((row, index) => (
              <li key={row.key} className="flex items-end gap-2">
                <div className="grid w-24 shrink-0 gap-1.5">
                  <Label
                    htmlFor={`${ids}-dkim-selector-${row.key}`}
                    className="text-xs text-muted-foreground"
                  >
                    Selector
                  </Label>
                  <Input
                    id={`${ids}-dkim-selector-${row.key}`}
                    className="h-9 font-mono"
                    autoComplete="off"
                    spellCheck={false}
                    value={row.selector}
                    onChange={(event) => {
                      const value = event.target.value;
                      setDkimRows((current) =>
                        current.map((entry) =>
                          entry.key === row.key
                            ? { ...entry, selector: value }
                            : entry,
                        ),
                      );
                    }}
                    disabled={submitting}
                  />
                </div>
                <div className="grid min-w-0 flex-1 gap-1.5">
                  <Label
                    htmlFor={`${ids}-dkim-target-${row.key}`}
                    className="text-xs text-muted-foreground"
                  >
                    Points at
                  </Label>
                  <Input
                    id={`${ids}-dkim-target-${row.key}`}
                    className="h-9 font-mono"
                    autoComplete="off"
                    spellCheck={false}
                    value={row.target}
                    onChange={(event) => {
                      const value = event.target.value;
                      setDkimRows((current) =>
                        current.map((entry) =>
                          entry.key === row.key
                            ? { ...entry, target: value }
                            : entry,
                        ),
                      );
                    }}
                    disabled={submitting}
                  />
                </div>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-lg"
                  aria-label={`Remove DKIM row ${index + 1}`}
                  onClick={() =>
                    setDkimRows((current) =>
                      current.filter((entry) => entry.key !== row.key),
                    )
                  }
                  disabled={submitting}
                >
                  <X aria-hidden="true" />
                </Button>
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-xs text-muted-foreground">
            Your provider shows the selector and key after you enable DKIM — use
            Add DKIM key on the Email card.
          </p>
        )}
      </div>

      {problem ? (
        <p role="alert" className="text-xs text-warning">
          {problem}
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
