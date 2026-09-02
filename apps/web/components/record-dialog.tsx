"use client";

import { type FormEvent, useState } from "react";

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
import {
  RecordType,
  type Record as DNSRecord,
} from "@/gen/dns/v1/dns_pb";
import { describeError } from "@/lib/errors";

export type RecordDraft = {
  name: string;
  type: RecordType;
  ttl: number;
  value: string;
};

type RecordDialogProps = {
  open: boolean;
  zoneName: string;
  record?: DNSRecord;
  onOpenChange: (open: boolean) => void;
  onSubmit: (draft: RecordDraft) => Promise<void>;
};

type RecordField = "name" | "type" | "ttl" | "value";

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

const typeNames: Record<RecordType, string> = {
  [RecordType.UNSPECIFIED]: "—",
  [RecordType.A]: "A",
  [RecordType.AAAA]: "AAAA",
  [RecordType.CNAME]: "CNAME",
  [RecordType.MX]: "MX",
  [RecordType.TXT]: "TXT",
  [RecordType.NS]: "NS",
  [RecordType.SRV]: "SRV",
  [RecordType.CAA]: "CAA",
  [RecordType.SOA]: "SOA",
};

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

export function recordTypeName(type: RecordType): string {
  return typeNames[type] ?? "UNKNOWN";
}

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
}: RecordDialogProps) {
  const [name, setName] = useState(record?.name ?? "@");
  const [type, setType] = useState(record?.type ?? RecordType.A);
  const [ttl, setTTL] = useState(String(record?.ttl ?? 300));
  const [value, setValue] = useState(record?.value ?? "");
  const [error, setError] = useState<RecordFormError>();
  const [submitting, setSubmitting] = useState(false);
  const guidance = valueGuidance[type as (typeof recordTypes)[number]];

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

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg">
        <form className="contents" onSubmit={handleSubmit}>
          <DialogHeader>
            <DialogTitle>{record ? "Edit record" : "Add a record"}</DialogTitle>
            <DialogDescription>
              {record ? "Update" : "Publish"} an authoritative answer in{" "}
              <span className="font-mono text-foreground">{zoneName}</span>.
            </DialogDescription>
          </DialogHeader>

          <div className="grid gap-4 py-1">
            {error && !error.field ? (
              <p role="alert" className="text-xs text-destructive">
                {error.message}
              </p>
            ) : null}
            <div className="grid gap-2 sm:grid-cols-[1fr_8rem]">
              <div className="grid gap-2">
                <Label htmlFor="record-name">Name</Label>
                <Input
                  id="record-name"
                  autoComplete="off"
                  autoFocus
                  placeholder="@ or www"
                  value={name}
                  onChange={(event) => {
                    setName(event.target.value);
                    if (error?.field === "name") {
                      setError(undefined);
                    }
                  }}
                  aria-invalid={error?.field === "name" || undefined}
                  aria-describedby={
                    error?.field === "name" ? "record-name-error" : undefined
                  }
                  disabled={submitting}
                />
                {error?.field === "name" ? (
                  <p
                    id="record-name-error"
                    role="alert"
                    className="text-xs text-destructive"
                  >
                    {error.message}
                  </p>
                ) : null}
              </div>
              <div className="grid gap-2">
                <Label htmlFor="record-type">Type</Label>
                <Select
                  value={String(type)}
                  onValueChange={(next) => {
                    setType(Number(next) as RecordType);
                    if (error?.field === "type") {
                      setError(undefined);
                    }
                  }}
                  disabled={submitting}
                >
                  <SelectTrigger
                    id="record-type"
                    className="w-full"
                    aria-invalid={error?.field === "type" || undefined}
                    aria-describedby={
                      error?.field === "type" ? "record-type-error" : undefined
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
                {error?.field === "type" ? (
                  <p
                    id="record-type-error"
                    role="alert"
                    className="text-xs text-destructive"
                  >
                    {error.message}
                  </p>
                ) : null}
              </div>
            </div>

            <div className="grid gap-2">
              <Label htmlFor="record-value">Value</Label>
              <Input
                id="record-value"
                className="font-mono"
                autoComplete="off"
                placeholder={guidance?.placeholder}
                value={value}
                onChange={(event) => {
                  setValue(event.target.value);
                  if (error?.field === "value") {
                    setError(undefined);
                  }
                }}
                aria-invalid={error?.field === "value" || undefined}
                aria-describedby={
                  error?.field === "value"
                    ? "record-value-error"
                    : "record-value-help"
                }
                disabled={submitting}
              />
              {error?.field === "value" ? (
                <p
                  id="record-value-error"
                  role="alert"
                  className="text-xs text-destructive"
                >
                  {error.message}
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
                    if (error?.field === "ttl") {
                      setError(undefined);
                    }
                  }}
                  aria-invalid={error?.field === "ttl" || undefined}
                  aria-describedby={
                    error?.field === "ttl" ? "record-ttl-error" : undefined
                  }
                  className="pr-14 font-mono tabular-nums"
                  disabled={submitting}
                />
                <span className="pointer-events-none absolute inset-y-0 right-2.5 flex items-center text-xs text-muted-foreground">
                  sec
                </span>
              </div>
              {error?.field === "ttl" ? (
                <p
                  id="record-ttl-error"
                  role="alert"
                  className="text-xs text-destructive"
                >
                  {error.message}
                </p>
              ) : null}
            </div>
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
            <Button
              type="submit"
              disabled={
                submitting ||
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
