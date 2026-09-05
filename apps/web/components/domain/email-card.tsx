import Link from "next/link";
import type { ReactNode } from "react";

import { RelativeTime } from "@/components/app/relative-time";
import {
  Card,
  CardContent,
  CardFooter,
  CardHeader,
  CardLink,
  CardTitle,
} from "@/components/ui/card";
import { StatusPill, type StatusPillTone } from "@/components/ui/status-pill";
import {
  ForwarderKind,
  MailDomainState,
  type Forwarder,
  type GetMailStatusResponse,
  type Mailbox,
  type MailDomain,
  type QueueSummary,
} from "@/gen/mail/v1/mail_pb";
import { stripDot } from "@/lib/dns-values";
import { plural } from "@/lib/format";
import {
  clientAutoconfigRecords,
  deliveryLine,
  formatBytes,
  mailNotPublished,
} from "@/lib/platform-model";
import type { EmailStatus } from "@/lib/zone-model";

export type EmailCardProps = {
  zoneName: string;
  email: EmailStatus;
  /** Static replacement for the ticking RelativeTime (used by the landing frame). */
  updatedLabel?: ReactNode;
  onRoute?: () => void;
  onAddDkim?: () => void;
  onShowRecords?: () => void;
  /** Rendered inside CardFooter instead of the callback links. */
  footer?: ReactNode;

  /* Mail engine (§8.4). Every line below reads a field the mail facade returned. */
  mailDomain?: MailDomain;
  mailboxes?: Mailbox[];
  forwarders?: Forwarder[];
  queue?: QueueSummary;
  mailConfigured?: boolean;
  mailStatus?: GetMailStatusResponse;
  onBind?: () => void;
  onUnbind?: () => void;
  onAddMailbox?: () => void;
  onAddForwarder?: () => void;
  onOpenQueue?: () => void;
  onReapplyDns?: () => void;
};

const pills: Record<
  EmailStatus["kind"],
  { tone: StatusPillTone; label: string }
> = {
  routed: { tone: "success", label: "ROUTED" },
  partial: { tone: "warning", label: "PARTIAL" },
  no_mail: { tone: "muted", label: "NO MAIL" },
  not_set_up: { tone: "muted", label: "NOT SET UP" },
};

const listRowClassName =
  "text-ui flex items-center justify-between gap-3 rounded-lg bg-fill px-3 py-2.5";

function Row({ label, children }: { label: ReactNode; children: ReactNode }) {
  return (
    <div className="flex justify-between gap-4">
      <span className="text-muted-foreground">{label}</span>
      <span className="min-w-0 text-right break-words">{children}</span>
    </div>
  );
}

function protectionValue(email: EmailStatus) {
  const entries: Array<[label: string, set: boolean]> = [
    ["SPF", email.protection.spf],
    ["DKIM", email.protection.dkim],
    ["DMARC", email.protection.dmarc],
  ];
  const set = entries.filter(([, ok]) => ok).map(([label]) => label);
  const missing = entries.filter(([, ok]) => !ok).map(([label]) => label);

  if (!missing.length) {
    return <span className="text-success">SPF, DKIM, DMARC set</span>;
  }
  if (set.length) {
    return (
      <span className="text-warning">
        {set.join(", ")} set · no {missing.join(", ")}
      </span>
    );
  }
  return (
    <span className={email.mx.length ? "text-warning" : "text-muted-foreground"}>
      Not set
    </span>
  );
}

/* -------------------------------------------------------------------------- */
/* The records-only card (phase 1, plus the engine's honest notes)             */
/* -------------------------------------------------------------------------- */

function RecordsCard({
  email,
  updatedLabel,
  onRoute,
  onAddDkim,
  onShowRecords,
  footer,
  note,
  setupParagraph,
  extraLinks,
}: {
  email: EmailStatus;
  updatedLabel?: ReactNode;
  onRoute?: () => void;
  onAddDkim?: () => void;
  onShowRecords?: () => void;
  footer?: ReactNode;
  note?: ReactNode;
  setupParagraph?: ReactNode;
  extraLinks?: ReactNode;
}) {
  const pill = pills[email.kind];
  const hasRecords =
    email.mx.length > 0 ||
    email.spf.records.length > 0 ||
    email.dkim.length > 0 ||
    email.dmarc.record !== undefined;
  const defaultSetupParagraph = (
    <p className="text-subtle leading-[1.55]">
      No mail servers yet. Pick your provider and the MX, SPF and DMARC records
      are written in one go.
    </p>
  );
  const links = (
    <>
      {extraLinks}
      {onRoute ? (
        <CardLink onClick={onRoute}>
          {email.mx.length ? "Change routing" : "Route email"}
        </CardLink>
      ) : null}
      {onAddDkim ? <CardLink onClick={onAddDkim}>Add DKIM key</CardLink> : null}
      {onShowRecords && hasRecords ? (
        <CardLink onClick={onShowRecords}>Show records</CardLink>
      ) : null}
    </>
  );
  const showFooter =
    footer !== undefined ||
    extraLinks !== undefined ||
    onRoute ||
    onAddDkim ||
    (onShowRecords && hasRecords);

  return (
    <Card>
      <CardHeader>
        <CardTitle>Email</CardTitle>
        <StatusPill tone={pill.tone}>{pill.label}</StatusPill>
      </CardHeader>

      <CardContent>
        {email.mx.length ? (
          <>
            <p className="text-xs text-muted-foreground">
              {email.mx.length} mail {plural(email.mx.length, "server")}
              {email.provider ? ` · ${email.provider}` : ""}
            </p>
            <div className="flex flex-col gap-1.5">
              {email.mx.map((mx) => (
                <div key={mx.recordId} className={listRowClassName}>
                  <span className="truncate font-mono text-[13px]">
                    {mx.host === "" ? "— (null MX: rejects all mail)" : mx.host}
                  </span>
                  <span className="text-xs whitespace-nowrap text-muted-foreground">
                    priority {mx.priority}
                  </span>
                </div>
              ))}
            </div>
          </>
        ) : null}

        {email.kind === "no_mail" && !email.mx.length ? (
          <p className="text-subtle">
            This domain declares that it sends no mail (SPF <code>-all</code>).
          </p>
        ) : null}

        {email.kind === "not_set_up"
          ? (setupParagraph ?? defaultSetupParagraph)
          : null}

        <div className="mt-1.5 flex flex-col gap-2">
          <Row label="Spam protection">{protectionValue(email)}</Row>
          {email.dmarc.record ? (
            <Row label="DMARC policy">
              {email.dmarc.parsed?.valid && email.dmarc.parsed.policy ? (
                <span className="font-mono">p={email.dmarc.parsed.policy}</span>
              ) : (
                <span className="text-warning">invalid</span>
              )}
            </Row>
          ) : null}
          <Row label="Last changed">
            {updatedLabel ?? <RelativeTime date={email.updatedAt} />}
          </Row>
        </div>

        {email.kind === "partial" && !email.mx.length
          ? (setupParagraph ?? defaultSetupParagraph)
          : null}

        {note ? (
          <p className="text-xs text-muted-foreground leading-[1.55]">{note}</p>
        ) : null}
      </CardContent>

      {showFooter ? <CardFooter>{footer ?? links}</CardFooter> : null}
    </Card>
  );
}

/* -------------------------------------------------------------------------- */
/* The bound-domain card                                                       */
/* -------------------------------------------------------------------------- */

function forwarderTargets(forwarder: Forwarder, zoneName: string): string {
  const targets = forwarder.targets;
  if (!targets.length) {
    return "—";
  }
  const first =
    forwarder.kind === ForwarderKind.ALIAS &&
    targets[0].toLowerCase().endsWith(`@${zoneName.toLowerCase()}`)
      ? `${targets[0].slice(0, targets[0].lastIndexOf("@"))}@`
      : targets[0];
  return targets.length > 1 ? `${first} +${targets.length - 1}` : first;
}

function queueValue(queue: QueueSummary) {
  if (!queue.available) {
    return <span className="text-muted-foreground">Not available</span>;
  }
  const waiting = queue.scheduled;
  const failing = queue.failing + queue.failed;
  if (!waiting && !failing) {
    return "Nothing waiting";
  }
  return (
    <span className={failing ? "text-warning" : undefined}>
      {waiting} waiting{failing ? ` · ${failing} failing` : ""}
    </span>
  );
}

function MailCard(props: EmailCardProps) {
  const {
    zoneName,
    email,
    mailDomain,
    mailboxes,
    forwarders,
    queue,
    mailStatus,
    footer,
    onUnbind,
    onAddMailbox,
    onAddForwarder,
    onOpenQueue,
    onReapplyDns,
  } = props;
  if (!mailDomain) {
    return null;
  }

  const state = mailDomain.state;
  const transitional =
    state === MailDomainState.BINDING || state === MailDomainState.UNBINDING;
  const zoneMissing = mailDomain.zoneMissing;

  const pill: { tone: StatusPillTone; label: string; pulse?: boolean } =
    state === MailDomainState.BINDING
      ? { tone: "info", label: "BINDING", pulse: true }
      : state === MailDomainState.UNBINDING
        ? { tone: "info", label: "UNBINDING", pulse: true }
        : zoneMissing
          ? { tone: "warning", label: "DEGRADED" }
          : {
              tone:
                !mailDomain.recordsInSync ||
                mailDomain.dkimPending ||
                (queue?.failing ?? 0) > 0 ||
                state === MailDomainState.DEGRADED
                  ? "warning"
                  : mailDomain.mailboxCount === 0
                    ? "info"
                    : "success",
              label: "HOSTED",
            };

  const spamProtection = mailDomain.dkimPending ? (
    <span className="text-warning">DKIM keys still generating</span>
  ) : (
    protectionValue(email)
  );

  const shownMailboxes = (mailboxes ?? []).slice(0, 3);
  const moreMailboxes = mailDomain.mailboxCount - shownMailboxes.length;

  // Both read the engine's own record set and delivery counters: what is published,
  // and what the mail server's delivery events actually said over the window it
  // measured. Neither line appears without a field behind it.
  const autoconfig = clientAutoconfigRecords(mailDomain);
  const notPublished = mailNotPublished(mailDomain);
  const delivery = deliveryLine(mailDomain);

  const body = (() => {
    if (transitional) {
      return (
        <p className="text-subtle leading-[1.55]">
          {state === MailDomainState.BINDING
            ? "Setting up mail on the server…"
            : "Removing…"}
        </p>
      );
    }
    if (zoneMissing) {
      return (
        <p className="text-subtle leading-[1.55]">
          This domain&apos;s zone no longer exists. Unbind mail to release it.
        </p>
      );
    }
    return (
      <>
        {/* mailbox_count is 0 both for an empty domain and for one the mail
            server did not answer for. counts_available separates them; the
            engine's own reason is already rendered as the card's status. */}
        {!mailDomain.countsAvailable ? (
          <p className="text-subtle leading-[1.55] text-warning">
            The mail server didn&apos;t report this domain&apos;s mailboxes.
          </p>
        ) : mailDomain.mailboxCount === 0 ? (
          <p className="text-subtle leading-[1.55]">
            Domain is bound; no mailboxes yet.
          </p>
        ) : (
          <>
            <p className="text-xs text-muted-foreground">
              {mailDomain.mailboxCount}{" "}
              {plural(mailDomain.mailboxCount, "mailbox", "mailboxes")}
              {mailDomain.usedBytes > BigInt(0)
                ? ` · ${formatBytes(mailDomain.usedBytes)}`
                : ""}
            </p>
            {shownMailboxes.length ? (
              <div className="flex flex-col gap-1.5">
                {shownMailboxes.map((mailbox) => (
                  <div key={mailbox.id} className={listRowClassName}>
                    <span className="truncate font-mono text-[13px]">
                      {mailbox.address}
                    </span>
                    <span className="text-xs whitespace-nowrap text-muted-foreground">
                      {formatBytes(mailbox.usedBytes)}
                    </span>
                  </div>
                ))}
                {moreMailboxes > 0 ? (
                  <p className="text-xs">
                    <Link
                      href={`/mailboxes?domain=${encodeURIComponent(zoneName)}`}
                      className="link"
                    >
                      +{moreMailboxes} more → Mailboxes
                    </Link>
                  </p>
                ) : null}
              </div>
            ) : null}
          </>
        )}

        {mailDomain.forwarderCount > 0 ? (
          <>
            <p className="text-xs text-muted-foreground">
              {mailDomain.forwarderCount}{" "}
              {plural(mailDomain.forwarderCount, "forwarder")}
            </p>
            {forwarders?.length ? (
              <div className="flex flex-col gap-1.5">
                {forwarders.slice(0, 3).map((forwarder) => (
                  <div key={forwarder.id} className={listRowClassName}>
                    <span className="truncate font-mono text-[13px]">
                      {forwarder.localPart}@ →{" "}
                      {forwarderTargets(forwarder, zoneName)}
                    </span>
                    <span className="text-xs whitespace-nowrap text-muted-foreground">
                      {forwarder.kind === ForwarderKind.ALIAS ? "alias" : "list"}
                    </span>
                  </div>
                ))}
              </div>
            ) : null}
          </>
        ) : null}

        <div className="mt-1.5 flex flex-col gap-2">
          <Row label="Spam protection">{spamProtection}</Row>
          <Row label="DMARC policy">
            {mailDomain.dmarcPolicy ? (
              <span className="font-mono">p={mailDomain.dmarcPolicy}</span>
            ) : (
              "—"
            )}
          </Row>
          <Row label="Reports">
            <span className="inline-flex flex-col items-end">
              <span className="font-mono break-all">
                {mailDomain.reportAddress || "—"}
              </span>
              {/* has_postmaster is false for a domain the mail server did not
                  answer for too, so it is only a statement when the counts
                  behind it were actually read. */}
              {mailDomain.countsAvailable && !mailDomain.hasPostmaster ? (
                <span className="text-xs text-warning">
                  create that mailbox or a forwarder
                </span>
              ) : null}
            </span>
          </Row>
          <Row label="Records">
            <span className="inline-flex flex-col items-end gap-1">
              {mailDomain.recordsInSync ? (
                <span className="text-success">
                  MX, SPF, DKIM and DMARC in place
                </span>
              ) : (
                <>
                  <span className="text-warning">Drifted</span>
                  {onReapplyDns ? (
                    <CardLink onClick={onReapplyDns}>Re-apply</CardLink>
                  ) : null}
                </>
              )}
              {autoconfig.length ? (
                <span className="text-xs text-muted-foreground">
                  + {autoconfig.length} mail client setup{" "}
                  {plural(autoconfig.length, "record")}
                </span>
              ) : null}
            </span>
          </Row>
          {queue ? <Row label="Queue">{queueValue(queue)}</Row> : null}
          {delivery ? (
            <Row label={`${delivery.label} · ${delivery.window}`}>
              <span title={delivery.title}>{delivery.text}</span>
            </Row>
          ) : null}
        </div>

        {/* The engine's own sentence about why there are no counts — shown only when
            there are none. With a delivery-event receiver configured the row above
            carries the real figures under the window the engine measured. */}
        {!delivery && mailStatus?.deliveryStatsNote ? (
          <p className="text-xs text-muted-foreground leading-[1.55]">
            {mailStatus.deliveryStatsNote}
          </p>
        ) : null}
        <p
          className="text-xs text-muted-foreground leading-[1.55]"
          title={notPublished.why}
        >
          {notPublished.line}
        </p>
      </>
    );
  })();

  const links = transitional ? null : zoneMissing ? (
    onUnbind ? (
      <CardLink onClick={onUnbind}>Unbind mail…</CardLink>
    ) : null
  ) : (
    <>
      {onAddMailbox ? (
        <CardLink onClick={onAddMailbox}>Add mailbox</CardLink>
      ) : null}
      {onAddForwarder ? (
        <CardLink onClick={onAddForwarder}>Add forwarder</CardLink>
      ) : null}
      {onOpenQueue ? (
        <CardLink onClick={onOpenQueue}>Mail queue</CardLink>
      ) : null}
      {onUnbind ? <CardLink onClick={onUnbind}>Unbind mail…</CardLink> : null}
    </>
  );

  const degradedReason =
    !zoneMissing && state === MailDomainState.DEGRADED && mailDomain.reason
      ? mailDomain.reason
      : undefined;

  return (
    <Card>
      {/* BINDING -> HOSTED (and the DKIM keys finishing) arrive by polling, so the
          pill changes without the reader touching anything. Same live region as the
          website card, for the same reason. */}
      <span role="status" className="sr-only">
        {`${zoneName} email: ${pill.label.toLowerCase()}`}
        {degradedReason ? `. ${degradedReason}` : ""}
      </span>
      <CardHeader>
        <CardTitle>Email</CardTitle>
        <StatusPill
          tone={pill.tone}
          pulse={pill.pulse}
          aria-label={`Email ${pill.label}`}
        >
          {pill.label}
        </StatusPill>
      </CardHeader>

      <CardContent>
        {body}
        {degradedReason ? (
          <p className="text-xs text-warning leading-[1.55]">
            {degradedReason}
          </p>
        ) : null}
      </CardContent>

      {footer !== undefined || links ? (
        <CardFooter>{footer ?? links}</CardFooter>
      ) : null}
    </Card>
  );
}

/* -------------------------------------------------------------------------- */

/**
 * The email half of a domain: the zone's MX / SPF / DKIM / DMARC records when nothing
 * is bound, and the mail server's own view of the domain once it is. No delivery or
 * spam outcome is claimed anywhere — the queue row is the only live number, and it
 * says "Not available" when the server could not report it.
 */
export function EmailCard(props: EmailCardProps) {
  const {
    email,
    updatedLabel,
    onRoute,
    onAddDkim,
    onShowRecords,
    footer,
    mailDomain,
    mailConfigured = false,
    mailStatus,
    onBind,
  } = props;

  if (mailDomain) {
    return <MailCard {...props} />;
  }

  const records = {
    email,
    updatedLabel,
    onRoute,
    onAddDkim,
    onShowRecords,
    footer,
  };

  // Nothing is known about the mail engine (the landing frame, or the status has not
  // loaded yet): the phase-1 card, verbatim.
  if (mailStatus === undefined) {
    return <RecordsCard {...records} />;
  }

  if (!mailConfigured) {
    return (
      <RecordsCard {...records} note="Mail isn't configured — records only." />
    );
  }

  if (email.mx.length) {
    const routedTo = email.provider ?? stripDot(email.mx[0].host);
    return (
      <RecordsCard
        {...records}
        note={
          routedTo
            ? `Routed to ${routedTo}. Host mailboxes here instead?`
            : "Host mailboxes here instead?"
        }
        extraLinks={
          onBind ? (
            <CardLink onClick={onBind}>Host mail here</CardLink>
          ) : null
        }
      />
    );
  }

  return (
    <RecordsCard
      {...records}
      setupParagraph={
        <p className="text-subtle leading-[1.55]">
          No mail yet. Add a mailbox and the MX, SPF, DKIM and DMARC records are
          written for you.
        </p>
      }
      extraLinks={
        <>
          {onBind ? <CardLink onClick={onBind}>Host mail here</CardLink> : null}
          {onRoute ? (
            <CardLink onClick={onRoute}>Route elsewhere</CardLink>
          ) : null}
        </>
      }
      // The engine links above replace the phase-1 routing link.
      onRoute={undefined}
    />
  );
}
