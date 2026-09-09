"use client";

import {
  type ChangeEvent,
  type FormEvent,
  type ReactNode,
  useState,
} from "react";

import { DismissGuardNote } from "@/components/app/dismiss-guard-note";
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
import { Textarea } from "@/components/ui/textarea";
import {
  RecordType,
  type Record as DNSRecord,
} from "@/gen/dns/v1/dns_pb";
import {
  isPrintableTxt,
  type RecordDraft,
  recordTypeName,
} from "@/lib/dns-values";
import { describeError } from "@/lib/errors";

// RecordDraft and recordTypeName live in lib/dns-values.ts (pure, importable from
// server code); they stay exported here so existing import sites keep working.
export type { RecordDraft };
export { recordTypeName };

type RecordField = "name" | "type" | "ttl" | "value";

type RecordValidation = { field: RecordField; message: string };

type RecordDialogProps = {
  open: boolean;
  zoneName: string;
  record?: DNSRecord;
  onOpenChange: (open: boolean) => void;
  onSubmit: (draft: RecordDraft) => Promise<void>;
  /** Seeds the fields when adding a record (ignored when `record` is given). */
  initial?: Partial<RecordDraft>;
  title?: string;
  description?: ReactNode;
  /** Renders the type select read-only, for flows that write one specific type. */
  lockType?: boolean;
  namePlaceholder?: string;
  /** Evaluated on every render; blocks submit while it returns an error. */
  validate?: (draft: RecordDraft) => RecordValidation | undefined;
};

type RecordFormError = {
  field?: RecordField;
  message: string;
};

const recordTypes = [
  RecordType.A,
  RecordType.AAAA,
  RecordType.CNAME,
  RecordType.MX,
  RecordType.TXT,
  RecordType.NS,
  RecordType.SRV,
  RecordType.CAA,
] as const;

const valueGuidance: Record<
  (typeof recordTypes)[number],
  { placeholder: string; help: string }
> = {
  [RecordType.A]: {
    placeholder: "192.0.2.10",
    help: "An IPv4 address.",
  },
  [RecordType.AAAA]: {
    placeholder: "2001:db8::10",
    help: "An IPv6 address.",
  },
  [RecordType.CNAME]: {
    placeholder: "target.example.net",
    help: "A canonical hostname; not valid at the zone apex.",
  },
  [RecordType.MX]: {
    placeholder: "10 mail.example.com",
    help: "Priority followed by a mail server hostname.",
  },
  [RecordType.TXT]: {
    placeholder: "v=spf1 -all",
    help: "Quotes are added automatically when omitted.",
  },
  [RecordType.NS]: {
    placeholder: "ns1.example.net",
    help: "A nameserver hostname, usually for a delegated child zone.",
  },
  [RecordType.SRV]: {
    placeholder: "10 5 443 service.example.com",
    help: "Priority, weight, port, then target hostname.",
  },
  [RecordType.CAA]: {
    placeholder: '0 issue "letsencrypt.org"',
    help: "Flags, tag, then certificate-authority value.",
  },
};

function describeRecordError(error: unknown): RecordFormError {
  const message = describeError(error);
  const match = /^(name|type|ttl|value):\s*(.+)$/.exec(message);
  if (!match) {
    return { message };
  }
  return {
    field: match[1] as RecordField,
    message: match[2],
  };
}

export function RecordDialog({
  open,
  zoneName,
  record,
  onOpenChange,
  onSubmit,
  initial,
  title,
  description,
  lockType,
  namePlaceholder,
  validate,
}: RecordDialogProps) {
  const [name, setName] = useState(record?.name ?? initial?.name ?? "@");
  const [type, setType] = useState(
    record?.type ?? initial?.type ?? RecordType.A,
  );
  const [ttl, setTTL] = useState(String(record?.ttl ?? initial?.ttl ?? 300));
  const [value, setValue] = useState(record?.value ?? initial?.value ?? "");
  const [touched, setTouched] = useState<Partial<Record<RecordField, boolean>>>(
    {},
  );
  const [error, setError] = useState<RecordFormError>();
  const [submitting, setSubmitting] = useState(false);
  const guidance = valueGuidance[type as (typeof recordTypes)[number]];

  // A control character in a TXT value is almost always a paste accident — a key
  // that wrapped across lines in another provider's UI carries the newline or tab
  // with it. The server stores those faithfully now (it writes DNS presentation
  // escapes, so a tab is published as a tab), which means accepting one here would
  // publish a record that is subtly not what the customer meant and gives no
  // symptom until a verifier disagrees. Saying so at the point of paste is kinder
  // than storing it correctly and wrong.
  const printable: RecordValidation | undefined =
    type === RecordType.TXT && value.trim() && !isPrintableTxt(value)
      ? {
          field: "value",
          message:
            "value must be printable text (no control characters or line breaks)",
        }
      : undefined;
  const validation =
    validate?.({ name, type, ttl: Number(ttl), value }) ?? printable;
  // Only surface the message once the offending field has been edited.
  const shown: RecordFormError | undefined =
    error ??
    (validation && touched[validation.field]
      ? { field: validation.field, message: validation.message }
      : undefined);

  function markTouched(field: RecordField) {
    setTouched((current) =>
      current[field] ? current : { ...current, [field]: true },
    );
  }

  function clearFieldError(field: RecordField) {
    if (error?.field === field) {
      setError(undefined);
    }
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(undefined);
    setSubmitting(true);
    try {
      await onSubmit({
        name,
        type,
        ttl: Number(ttl),
        value,
      });
      onOpenChange(false);
    } catch (caught) {
      setError(describeRecordError(caught));
    } finally {
      setSubmitting(false);
    }
  }

  const valueProps = {
    id: "record-value",
    autoComplete: "off" as const,
    placeholder: guidance?.placeholder,
    value,
    onChange: (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      setValue(event.target.value);
      markTouched("value");
      clearFieldError("value");
    },
    "aria-invalid": shown?.field === "value" || undefined,
    "aria-describedby":
      shown?.field === "value" ? "record-value-error" : "record-value-help",
    disabled: submitting,
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        // Dismissing mid-write would drop the error the request is about to return.
        if (!next && submitting) {
          return;
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
            <DialogTitle>
              {title ?? (record ? "Edit record" : "Add a record")}
            </DialogTitle>
            <DialogDescription>
              {description ?? (
                <>
                  {record ? "Update" : "Publish"} an authoritative answer in{" "}
                  <span className="font-mono text-foreground">{zoneName}</span>.
                </>
              )}
            </DialogDescription>
          </DialogHeader>

          <div className="grid gap-4 py-1">
            {shown && !shown.field ? (
              <p role="alert" className="text-xs text-destructive">
                {shown.message}
              </p>
            ) : null}
            <div className="grid gap-2 sm:grid-cols-[1fr_8rem]">
              <div className="grid gap-2">
                <Label htmlFor="record-name">Name</Label>
                <Input
                  id="record-name"
                  autoComplete="off"
                  autoFocus
                  placeholder={namePlaceholder ?? "@ or www"}
                  value={name}
                  onChange={(event) => {
                    setName(event.target.value);
                    markTouched("name");
                    clearFieldError("name");
                  }}
                  aria-invalid={shown?.field === "name" || undefined}
                  aria-describedby={
                    shown?.field === "name" ? "record-name-error" : undefined
                  }
                  disabled={submitting}
                />
                {shown?.field === "name" ? (
                  <p
                    id="record-name-error"
                    role="alert"
                    className="text-xs text-destructive"
                  >
                    {shown.message}
                  </p>
                ) : null}
              </div>
              <div className="grid gap-2">
                <Label htmlFor="record-type">Type</Label>
                <Select
                  value={String(type)}
                  onValueChange={(next) => {
                    setType(Number(next) as RecordType);
                    markTouched("type");
                    clearFieldError("type");
                  }}
                  disabled={submitting || lockType}
                >
                  <SelectTrigger
                    id="record-type"
                    className="w-full"
                    aria-invalid={shown?.field === "type" || undefined}
                    aria-describedby={
                      shown?.field === "type" ? "record-type-error" : undefined
                    }
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {recordTypes.map((recordType) => (
                      <SelectItem key={recordType} value={String(recordType)}>
                        {recordTypeName(recordType)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {shown?.field === "type" ? (
                  <p
                    id="record-type-error"
                    role="alert"
                    className="text-xs text-destructive"
                  >
                    {shown.message}
                  </p>
                ) : null}
              </div>
            </div>

            <div className="grid gap-2">
              <Label htmlFor="record-value">Value</Label>
              {type === RecordType.TXT ? (
                <Textarea mono {...valueProps} />
              ) : (
                <Input className="font-mono" {...valueProps} />
              )}
              {shown?.field === "value" ? (
                <p
                  id="record-value-error"
                  role="alert"
                  className="text-xs text-destructive"
                >
                  {shown.message}
                </p>
              ) : (
                <p
                  id="record-value-help"
                  className="text-xs text-muted-foreground"
                >
                  {guidance?.help}
                </p>
              )}
            </div>

            <div className="grid max-w-40 gap-2">
              <Label htmlFor="record-ttl">TTL</Label>
              <div className="relative">
                <Input
                  id="record-ttl"
                  type="number"
                  min={1}
                  max={2_147_483_647}
                  inputMode="numeric"
                  value={ttl}
                  onChange={(event) => {
                    setTTL(event.target.value);
                    markTouched("ttl");
                    clearFieldError("ttl");
                  }}
                  aria-invalid={shown?.field === "ttl" || undefined}
                  aria-describedby={
                    shown?.field === "ttl" ? "record-ttl-error" : undefined
                  }
                  className="pr-14 font-mono tabular-nums"
                  disabled={submitting}
                />
                <span className="pointer-events-none absolute inset-y-0 right-2.5 flex items-center text-xs text-muted-foreground">
                  sec
                </span>
              </div>
              {shown?.field === "ttl" ? (
                <p
                  id="record-ttl-error"
                  role="alert"
                  className="text-xs text-destructive"
                >
                  {shown.message}
                </p>
              ) : null}
            </div>
          </div>

          <DismissGuardNote active={submitting} action="Writing the record" />

          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={submitting}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={
                submitting ||
                Boolean(validation) ||
                !name.trim() ||
                !value.trim() ||
                !ttl ||
                Number(ttl) < 1
              }
            >
              {submitting
                ? record
                  ? "Saving…"
                  : "Publishing…"
                : record
                  ? "Save record"
                  : "Publish record"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
