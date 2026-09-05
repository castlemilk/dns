"use client";

import { useId, useState, type FormEvent } from "react";

import { usePlatform, useMailDomain } from "@/components/app/platform-provider";
import { RecordChangeList } from "@/components/app/record-change-list";
import { RouteEmailForm } from "@/components/guided/route-email-form";
import { MailboxCreatedDialog } from "@/components/mail/mailbox-created-dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { StatusPill } from "@/components/ui/status-pill";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import { MailDomainState } from "@/gen/mail/v1/mail_pb";
import type { RecordChange } from "@/gen/platform/v1/platform_pb";
import { plural } from "@/lib/format";
import { describeError } from "@/lib/errors";
import { getMailClient } from "@/lib/platform-client";

export type EmailStepProps = {
  zone: Zone;
  onContinue(): void;
  onSkip(): void;
};

type Credentials = {
  address: string;
  password: string;
  imapHost: string;
  imapPort: number;
  smtpHost: string;
  smtpPort: number;
};

const localPartPattern = /^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$/;

const stateLabels: Record<number, string> = {
  [MailDomainState.BINDING]: "BINDING",
  [MailDomainState.BOUND]: "HOSTED",
  [MailDomainState.DEGRADED]: "DEGRADED",
  [MailDomainState.UNBINDING]: "UNBINDING",
};

/**
 * The connect flow's Email step when the mail engine is configured: bind the domain to
 * the mail server and create the first mailbox in one submit. The generated password is
 * shown once, by `MailboxCreatedDialog`, and never leaves that dialog's props.
 */
export function EmailStep({ zone, onContinue, onSkip }: EmailStepProps) {
  const ids = useId();
  const { invalidate, refreshMailDomain } = usePlatform();
  const { domain } = useMailDomain(zone.id);

  const [mode, setMode] = useState<"here" | "elsewhere">("here");
  const [localPart, setLocalPart] = useState("hello");
  const [displayName, setDisplayName] = useState("");
  const [replace, setReplace] = useState(false);
  const [conflicts, setConflicts] = useState<RecordChange[]>([]);
  const [plan, setPlan] = useState<RecordChange[]>([]);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState<Credentials>();

  async function bindAndCreate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(undefined);

    const local = localPart.trim().toLowerCase();
    if (!localPartPattern.test(local)) {
      setError("Use letters, digits, dots, dashes and underscores.");
      return;
    }

    setBusy(true);
    try {
      const client = getMailClient();
      const request = {
        zoneId: zone.id,
        dmarcPolicy: "",
        replaceConflictingRecords: replace,
      };

      if (!domain) {
        const preview = await client.bindMailDomain({ ...request, dryRun: true });
        setPlan(preview.dnsPlan);
        setConflicts(preview.conflicts);
        if (preview.conflicts.length && !replace) {
          setBusy(false);
          return;
        }
        await client.bindMailDomain({ ...request, dryRun: false });
      }

      const mailbox = await client.createMailbox({
        zoneId: zone.id,
        localPart: local,
        displayName: displayName.trim(),
        quotaBytes: BigInt(0),
      });
      await invalidate();
      await refreshMailDomain(zone.id);
      setCreated({
        address: mailbox.mailbox?.address ?? `${local}@${zone.name}`,
        password: mailbox.password,
        imapHost: mailbox.imapHost,
        imapPort: mailbox.imapPort,
        smtpHost: mailbox.smtpHost,
        smtpPort: mailbox.smtpPort,
      });
    } catch (caught) {
      setError(describeError(caught));
    } finally {
      setBusy(false);
    }
  }

  const bound = domain !== undefined && created === undefined;

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-2.5">
        <h1 className="text-[30px] leading-[1.15] font-semibold break-words">
          Set up email for {zone.name}
        </h1>
        <p className="text-[15px] leading-[1.6] text-subtle">
          Mailboxes live on this mail server; MX, SPF, DKIM and DMARC are
          published in your zone automatically.
        </p>
      </div>

      {bound && domain ? (
        <div className="flex flex-col gap-4">
          <div className="flex flex-wrap items-center gap-2.5">
            <StatusPill
              tone={
                domain.state === MailDomainState.BOUND
                  ? "success"
                  : domain.state === MailDomainState.DEGRADED
                    ? "warning"
                    : "info"
              }
              pulse={
                domain.state === MailDomainState.BINDING ||
                domain.state === MailDomainState.UNBINDING
              }
              aria-label={stateLabels[domain.state] ?? "UNKNOWN"}
            >
              {stateLabels[domain.state] ?? "UNKNOWN"}
            </StatusPill>
            <span className="text-ui text-muted-foreground">
              {domain.state === MailDomainState.BINDING
                ? "Setting up mail on the server…"
                : domain.reason ||
                  `${domain.mailboxCount} ${plural(
                    domain.mailboxCount,
                    "mailbox",
                    "mailboxes",
                  )} on ${domain.mailHostname}.`}
            </span>
          </div>
          {domain.dkimPending ? (
            <p className="text-ui text-warning">
              DKIM keys are still being generated — the records are completed
              automatically within a minute.
            </p>
          ) : null}
          <div className="flex flex-wrap items-center justify-end gap-2.5">
            <Button
              type="button"
              variant="outline"
              size="lg"
              className="text-ui h-10 rounded-[7px] px-4"
              onClick={onSkip}
            >
              Skip
            </Button>
            <Button
              type="button"
              size="lg"
              className="text-ui h-10 rounded-[7px] px-4 font-semibold"
              onClick={onContinue}
            >
              Continue
            </Button>
          </div>
        </div>
      ) : (
        <>
          <RadioGroup
            value={mode}
            onValueChange={(next) => setMode(next as "here" | "elsewhere")}
            className="gap-3"
          >
            <div className="flex items-start gap-3 rounded-[10px] border border-line px-4 py-3">
              <RadioGroupItem value="here" id={`${ids}-here`} className="mt-1" />
              <Label
                htmlFor={`${ids}-here`}
                className="flex flex-col items-start gap-1"
              >
                <span className="text-ui font-medium">Host mail here</span>
                <span className="text-[13px] font-normal text-muted-foreground">
                  Create a mailbox on this platform&rsquo;s mail server.
                </span>
              </Label>
            </div>
            <div className="flex items-start gap-3 rounded-[10px] border border-line px-4 py-3">
              <RadioGroupItem
                value="elsewhere"
                id={`${ids}-elsewhere`}
                className="mt-1"
              />
              <Label
                htmlFor={`${ids}-elsewhere`}
                className="flex flex-col items-start gap-1"
              >
                <span className="text-ui font-medium">
                  Route to another provider
                </span>
                <span className="text-[13px] font-normal text-muted-foreground">
                  Write the MX, SPF and DMARC records your provider asks for.
                </span>
              </Label>
            </div>
          </RadioGroup>

          {mode === "here" ? (
            <form onSubmit={bindAndCreate} className="flex flex-col gap-5">
              <div className="grid gap-2">
                <Label htmlFor={`${ids}-local`}>First mailbox</Label>
                <div className="flex items-center gap-2">
                  <Input
                    id={`${ids}-local`}
                    className="h-10 font-mono"
                    autoComplete="off"
                    autoCapitalize="none"
                    spellCheck={false}
                    value={localPart}
                    onChange={(event) => setLocalPart(event.target.value)}
                  />
                  <span className="font-mono text-[13px] text-muted-foreground">
                    @{zone.name}
                  </span>
                </div>
              </div>

              <div className="grid gap-2">
                <Label htmlFor={`${ids}-name`}>
                  Display name{" "}
                  <span className="text-muted-foreground">(optional)</span>
                </Label>
                <Input
                  id={`${ids}-name`}
                  className="h-10"
                  autoComplete="off"
                  value={displayName}
                  onChange={(event) => setDisplayName(event.target.value)}
                />
              </div>

              {conflicts.length ? (
                <div className="flex flex-col gap-3 rounded-[10px] border border-warning/25 bg-warning/8 px-4 py-3.5">
                  <RecordChangeList changes={plan} conflicts={conflicts} />
                  <div className="flex items-start gap-2.5">
                    <Checkbox
                      id={`${ids}-replace`}
                      checked={replace}
                      onCheckedChange={(checked) => setReplace(checked === true)}
                    />
                    <Label htmlFor={`${ids}-replace`} className="font-normal">
                      Replace the {conflicts.length}{" "}
                      {plural(conflicts.length, "custom record")} listed above
                    </Label>
                  </div>
                </div>
              ) : null}

              {error ? (
                <p role="alert" className="text-ui text-destructive">
                  {error}
                </p>
              ) : null}

              <div className="flex flex-wrap items-center justify-end gap-2.5">
                <Button
                  type="button"
                  variant="outline"
                  size="lg"
                  className="text-ui h-10 rounded-[7px] px-4"
                  onClick={onSkip}
                >
                  Skip
                </Button>
                <Button
                  type="submit"
                  size="lg"
                  className="text-ui h-10 rounded-[7px] px-4 font-semibold"
                  disabled={busy || (conflicts.length > 0 && !replace)}
                >
                  {busy ? "Working…" : "Create mailbox and continue"}
                </Button>
              </div>
            </form>
          ) : (
            <RouteEmailForm
              zone={zone}
              layout="inline"
              submitLabel="Write records and continue"
              onApplied={onContinue}
              footerSlot={(submit) => (
                <div className="flex flex-wrap items-center justify-end gap-2.5">
                  <Button
                    type="button"
                    variant="outline"
                    size="lg"
                    className="text-ui h-10 rounded-[7px] px-4"
                    onClick={onSkip}
                  >
                    Skip
                  </Button>
                  {submit}
                </div>
              )}
            />
          )}
        </>
      )}

      {created ? (
        <MailboxCreatedDialog
          address={created.address}
          password={created.password}
          imapHost={created.imapHost}
          imapPort={created.imapPort}
          smtpHost={created.smtpHost}
          smtpPort={created.smtpPort}
          open
          onOpenChange={(open) => {
            if (!open) {
              setCreated(undefined);
              onContinue();
            }
          }}
        />
      ) : null}
    </div>
  );
}
