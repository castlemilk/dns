"use client";

import { LockKeyhole, TriangleAlert } from "lucide-react";
import { useState, type ReactNode } from "react";

import { CopyButton } from "@/components/app/copy-button";
import { PageHeader } from "@/components/app/page-header";
import { usePlatform } from "@/components/app/platform-provider";
import { RelativeTime } from "@/components/app/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusDot } from "@/components/ui/status-dot";
import type { GetBillingStatusResponse } from "@/gen/billing/v1/billing_pb";
import type { GetHostingStatusResponse } from "@/gen/hosting/v1/hosting_pb";
import type { GetMailStatusResponse } from "@/gen/mail/v1/mail_pb";
import { EngineKind, type EngineStatus } from "@/gen/platform/v1/platform_pb";
import { describeError } from "@/lib/errors";
import { plural, toDate } from "@/lib/format";
import { lockOperatorSession } from "@/lib/operator-session";
import { getHostingClient } from "@/lib/platform-client";
import { cn } from "@/lib/utils";

/**
 * What this deployment actually is: which engines are configured and reachable, what
 * they answered, and which environment variables an operator still has to set. Nothing
 * on this screen is derived or defaulted — every value is a field of
 * `GetPlatformStatus`, `GetHostingStatus`, `GetMailStatus` or `GetBillingStatus`, and a
 * section whose engine is not configured says so instead of showing blanks.
 *
 * The three-state rule from `platform-provider.tsx` holds everywhere:
 * `statusError && !status` → the control API itself did not answer (never
 * "isn't configured"); `!loaded` → skeleton; otherwise the real thing. A refresh error
 * after a good load is an inline note above the rows, never a replacement for them.
 */
export function SettingsPage() {
  const {
    status,
    statusLoaded,
    statusError,
    refreshStatus,
    hosting,
    mail,
    billing,
    engine,
  } = usePlatform();

  const unreachable = statusError !== undefined && status === undefined;

  return (
    <div className="flex flex-col gap-8">
      <PageHeader title="Settings" />

      <OperatorSection />

      <Section
        title="Engines"
        action={
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              void refreshStatus(true);
            }}
          >
            Re-check
          </Button>
        }
      >
        {unreachable ? (
          // Nothing is known about any engine, so the page says so rather than
          // implying every engine is missing (spec2 §8.1).
          <Alert>
            <TriangleAlert aria-hidden="true" className="text-warning" />
            <AlertTitle>The control API didn&apos;t answer</AlertTitle>
            <AlertDescription>
              <div className="text-soft">
                {statusError || "No reason was reported."}
              </div>
              <div className="mt-2">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    void refreshStatus(true);
                  }}
                >
                  Retry
                </Button>
              </div>
            </AlertDescription>
          </Alert>
        ) : !statusLoaded ? (
          <div className="flex flex-col gap-2">
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
          </div>
        ) : (
          <>
            {statusError ? (
              // The rows below are the last good answer; say so instead of
              // hiding them.
              <p className="text-ui text-muted-foreground" role="alert">
                Couldn&apos;t refresh: {statusError}
              </p>
            ) : null}
            <ul className="flex flex-col gap-2">
              {(status?.engines ?? []).map((entry) => (
                <EngineRow key={entry.kind} engine={entry} />
              ))}
            </ul>
          </>
        )}
      </Section>

      <Section title="Gateway">
        <EngineSection
          configured={hosting.configured}
          loaded={hosting.loaded}
          error={hosting.error}
          missingEnv={engine(EngineKind.HOSTING)?.missingEnv ?? []}
          notConfigured="No hosting engine is configured, so this control plane writes no website records."
        >
          {hosting.status ? (
            <GatewayFields status={hosting.status} />
          ) : (
            <p className="text-ui text-muted-foreground">
              The hosting engine didn&apos;t report its gateway.
            </p>
          )}
        </EngineSection>
      </Section>

      <Section title="Mail host">
        <EngineSection
          configured={mail.configured}
          loaded={mail.loaded}
          error={mail.error}
          missingEnv={engine(EngineKind.MAIL)?.missingEnv ?? []}
          notConfigured="No mail server is configured, so this control plane writes no email records and hosts no mailboxes."
        >
          {mail.status ? (
            <MailFields
              status={mail.status}
              engine={engine(EngineKind.MAIL)}
            />
          ) : (
            <p className="text-ui text-muted-foreground">
              The mail server didn&apos;t report its configuration.
            </p>
          )}
        </EngineSection>
      </Section>

      <Section title="Billing">
        <EngineSection
          configured={billing.configured}
          loaded={billing.loaded}
          error={billing.error}
          missingEnv={engine(EngineKind.BILLING)?.missingEnv ?? []}
          notConfigured="No billing provider is configured. Every domain keeps working; nothing is charged and no subscription state is shown."
        >
          {billing.status ? (
            <BillingFields status={billing.status} />
          ) : (
            <p className="text-ui text-muted-foreground">
              The billing provider didn&apos;t report its configuration.
            </p>
          )}
        </EngineSection>
      </Section>

      <Section title="Nameservers">
        {!statusLoaded ? (
          <Skeleton className="h-10 w-full" />
        ) : status?.nameservers.length ? (
          <div className="flex flex-col gap-2 rounded-[10px] border border-line px-4 py-3">
            <ul className="flex flex-col gap-1 font-mono text-[13px] text-soft">
              {status.nameservers.map((ns) => (
                <li key={ns} className="break-all">
                  {ns}
                </li>
              ))}
            </ul>
            <div className="self-start">
              <CopyButton
                value={status.nameservers.join("\n")}
                valueLabel="nameservers"
              />
            </div>
          </div>
        ) : (
          <p className="text-ui text-muted-foreground">
            This control plane reports no nameservers.
          </p>
        )}
      </Section>

      <Section title="Data">
        <div className="flex flex-col gap-2 rounded-[10px] border border-line px-4 py-3">
          <Field label="Zone files">
            Export/import zone files from each domain&apos;s Settings menu
          </Field>
          {statusLoaded && status ? (
            <>
              <Field label="Platform store">
                <span className="font-mono">
                  {status.platformStoreName || "—"}
                </span>
              </Field>
              {status.platformBackupRoute ? (
                <Field label="Backup route">
                  <span className="font-mono">
                    /internal/v1/platform-backup
                  </span>
                </Field>
              ) : null}
            </>
          ) : null}
        </div>
      </Section>

      <Section title="About">
        {!statusLoaded ? (
          <Skeleton className="h-20 w-full" />
        ) : status ? (
          <div className="flex flex-col gap-2 rounded-[10px] border border-line px-4 py-3">
            <Field label="Version">
              <span className="font-mono">{status.version || "—"}</span>
            </Field>
            <Field label="Started">
              {toDate(status.startedAt) ? (
                <RelativeTime date={toDate(status.startedAt)} />
              ) : (
                "—"
              )}
            </Field>
            <Field label="Mode">
              {status.production ? "production" : "development"}
            </Field>
            {status.billingInformational ? (
              <Field label="Billing policy">
                Informational only — nothing is locked out for an unbilled
                domain
              </Field>
            ) : null}
          </div>
        ) : (
          <p className="text-ui text-muted-foreground">
            The control API didn&apos;t answer.
          </p>
        )}
      </Section>
    </div>
  );
}

/* -------------------------------------------------------------------------- */
/* Layout primitives                                                           */
/* -------------------------------------------------------------------------- */

function Section({
  title,
  action,
  children,
}: {
  title: string;
  action?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-center justify-between gap-3">
        <h2 className="eyebrow">{title}</h2>
        {action}
      </div>
      {children}
    </section>
  );
}

function Field({
  label,
  children,
  tone,
}: {
  label: string;
  children: ReactNode;
  tone?: "warning";
}) {
  return (
    <div className="text-ui flex flex-col gap-0.5 sm:flex-row sm:items-baseline sm:gap-4">
      <span className="text-muted-foreground sm:w-44 sm:shrink-0">{label}</span>
      <span
        className={cn(
          "min-w-0 break-words",
          tone === "warning" ? "text-warning" : undefined,
        )}
      >
        {children}
      </span>
    </div>
  );
}

/**
 * The per-engine wrapper for the sections below the Engines list: skeleton while the
 * engine slice is loading, an honest sentence when it is not configured, the engine's
 * own error when the list failed, and the section body otherwise.
 */
function EngineSection({
  configured,
  loaded,
  error,
  missingEnv,
  notConfigured,
  children,
}: {
  configured: boolean;
  loaded: boolean;
  error?: string;
  missingEnv: string[];
  notConfigured: string;
  children: ReactNode;
}) {
  if (!loaded) {
    return <Skeleton className="h-24 w-full" />;
  }
  if (!configured) {
    return (
      <p className="text-ui text-muted-foreground">
        {notConfigured}
        {missingEnv.length ? (
          <>
            {" "}
            Set{" "}
            <span className="font-mono text-[12.5px] text-soft">
              {missingEnv.join(", ")}
            </span>{" "}
            and restart the control API.
          </>
        ) : null}
      </p>
    );
  }
  if (error) {
    return (
      <p className="text-ui text-warning" role="alert">
        {error}
      </p>
    );
  }
  return (
    <div className="flex flex-col gap-3 rounded-[10px] border border-line px-4 py-3">
      {children}
    </div>
  );
}

/* -------------------------------------------------------------------------- */
/* Operator session                                                            */
/* -------------------------------------------------------------------------- */

function OperatorSection() {
  return (
    <Section title="Operator session">
      <div className="flex flex-col gap-3 rounded-[10px] border border-line px-4 py-3 sm:flex-row sm:items-center sm:justify-between">
        <p className="text-ui text-muted-foreground">
          Unlocked in this tab · token stored in session storage
        </p>
        <Button
          variant="outline"
          size="sm"
          className="self-start"
          onClick={() => lockOperatorSession("manual")}
        >
          <LockKeyhole aria-hidden="true" />
          Lock
        </Button>
      </div>
    </Section>
  );
}

/* -------------------------------------------------------------------------- */
/* Engines                                                                     */
/* -------------------------------------------------------------------------- */

const engineNames: Record<number, string> = {
  [EngineKind.DNS]: "DNS control API",
  [EngineKind.HOSTING]: "Hosting",
  [EngineKind.MAIL]: "Mail",
  [EngineKind.BILLING]: "Billing",
};

const capabilityNames: Record<string, string> = {
  list_builds: "list builds",
  delete_domain: "delete domain",
  build_timestamps: "build timestamps",
  domain_readiness: "domain readiness",
  tenant_scoped: "tenant-scoped token",
  env_vars: "environment variables",
  domain_redirect: "host redirects",
  resolved_commit: "resolved commit",
};

const chipClassName =
  "rounded-md px-2 py-0.5 font-mono text-[12px] whitespace-nowrap";

function EngineRow({ engine }: { engine: EngineStatus }) {
  const name = engineNames[engine.kind] ?? "Engine";
  const checkedAt = toDate(engine.checkedAt);

  const tone = !engine.configured
    ? "muted"
    : engine.reachable
      ? "success"
      : "warning";

  const capabilities = Object.entries(engine.capabilities).sort(([a], [b]) =>
    a.localeCompare(b),
  );

  return (
    <li className="flex flex-col gap-1.5 rounded-[10px] border border-line px-4 py-3">
      <div className="flex flex-wrap items-center gap-2">
        <StatusDot tone={tone} />
        <span className="text-ui font-medium">{name}</span>
        {engine.provider ? (
          <span className="font-mono text-[12.5px] text-soft">
            ({engine.provider})
          </span>
        ) : null}
        {engine.version ? (
          <span className="font-mono text-[12.5px] text-muted-foreground">
            {engine.version}
          </span>
        ) : null}
      </div>

      {!engine.configured ? (
        <p className="text-ui text-muted-foreground">
          Not configured
          {engine.missingEnv.length ? (
            <>
              {" · set "}
              <span className="font-mono text-[12.5px] text-soft">
                {engine.missingEnv.join(", ")}
              </span>
            </>
          ) : null}
        </p>
      ) : engine.reachable ? (
        <p className="text-ui text-muted-foreground">
          Configured · reachable
          {checkedAt ? (
            <>
              , checked <RelativeTime date={checkedAt} />
            </>
          ) : null}
        </p>
      ) : (
        <p className="text-ui text-warning">
          Configured · unreachable{engine.reason ? `: ${engine.reason}` : ""}
        </p>
      )}

      {engine.endpointHost ? (
        <p className="font-mono text-[12.5px] text-muted-foreground break-all">
          {engine.endpointHost}
        </p>
      ) : null}

      {capabilities.length ? (
        <ul className="mt-0.5 flex flex-wrap gap-1.5">
          {capabilities.map(([key, supported]) => (
            <li
              key={key}
              className={cn(
                chipClassName,
                supported
                  ? "bg-success/10 text-success"
                  : "bg-fill text-muted-foreground",
              )}
            >
              {supported ? "" : "no "}
              {capabilityNames[key] ?? key}
            </li>
          ))}
          {engine.capabilities["tenant_scoped"] === false ? (
            // This chip is a sentence, not a capability name: it must wrap, or
            // it pushes the page past 360 px.
            <li
              className={cn(
                chipClassName,
                "bg-warning/10 text-warning whitespace-normal break-words",
              )}
            >
              token is admin-scoped — scope it with DH_API_TOKEN_TENANTS
            </li>
          ) : null}
        </ul>
      ) : null}
    </li>
  );
}

/* -------------------------------------------------------------------------- */
/* Gateway                                                                     */
/* -------------------------------------------------------------------------- */

function GatewayFields({ status }: { status: GetHostingStatusResponse }) {
  const { invalidate } = usePlatform();
  const [applying, setApplying] = useState(false);
  const [error, setError] = useState<string>();
  const pending = status.gatewayPendingAddresses;

  async function applyPending() {
    setError(undefined);
    setApplying(true);
    try {
      await getHostingClient().confirmGatewayAddresses({ addresses: pending });
      await invalidate();
    } catch (caught) {
      setError(describeError(caught));
    } finally {
      setApplying(false);
    }
  }

  return (
    <>
      {pending.length ? (
        <Alert>
          <TriangleAlert aria-hidden="true" className="text-warning" />
          <AlertTitle>A new gateway address set is waiting</AlertTitle>
          <AlertDescription>
            <div className="text-soft">
              The gateway hostname now resolves to{" "}
              <span className="font-mono break-all">{pending.join(", ")}</span>,
              which shares nothing with the current set. Apply them?
            </div>
            {toDate(status.gatewayPendingSince) ? (
              <div className="text-xs text-muted-foreground">
                Waiting since{" "}
                <RelativeTime date={toDate(status.gatewayPendingSince)} />
              </div>
            ) : null}
            {error ? (
              <div className="text-xs text-destructive" role="alert">
                {error}
              </div>
            ) : null}
            <div className="mt-2">
              <Button
                variant="outline"
                size="sm"
                disabled={applying}
                onClick={() => {
                  void applyPending();
                }}
              >
                {applying ? "Applying…" : "Apply the new gateway addresses"}
              </Button>
            </div>
          </AlertDescription>
        </Alert>
      ) : null}

      <div className="flex flex-col gap-2">
        <Field label="Hostname">
          <span className="font-mono">{status.gatewayHostname || "—"}</span>
        </Field>
        <Field label="Addresses">
          <span className="font-mono break-all">
            {status.gatewayAddresses.length
              ? status.gatewayAddresses.join(", ")
              : "—"}
          </span>
        </Field>
        <Field label="Source">{status.gatewaySource || "—"}</Field>
        <Field label="Resolver">
          <span className="font-mono">{status.gatewayResolver || "—"}</span>
        </Field>
        <Field label="Resolved">
          {toDate(status.gatewayResolvedAt) ? (
            <RelativeTime date={toDate(status.gatewayResolvedAt)} />
          ) : (
            "—"
          )}
        </Field>
        {status.gatewayError ? (
          <Field label="Last error" tone="warning">
            {status.gatewayError}
          </Field>
        ) : null}
        <Field label="Apps suffix">
          <span className="font-mono">{status.appsSuffix || "—"}</span>
        </Field>
        <Field label="Sites">
          {status.sites} {plural(status.sites, "site")}
          {status.activeDeploys
            ? ` · ${status.activeDeploys} ${plural(status.activeDeploys, "deploy")} running`
            : ""}
        </Field>
      </div>
    </>
  );
}

/* -------------------------------------------------------------------------- */
/* Mail host                                                                   */
/* -------------------------------------------------------------------------- */

function MailFields({
  status,
  engine,
}: {
  status: GetMailStatusResponse;
  engine?: EngineStatus;
}) {
  const checkedAt = toDate(engine?.checkedAt);
  const lastDeliveryEvent = toDate(status.receiverLastEventAt);
  return (
    <div className="flex flex-col gap-2">
      <Field label="Hostname">
        <span className="font-mono">{status.mailHostname || "—"}</span>
      </Field>
      <Field label="Edition">{status.edition || "not reported"}</Field>
      <Field label="Mailboxes per domain">
        {status.mailboxesPerDomain || "—"}
      </Field>
      {/* The sum covers every bound domain only when every per-domain read
          answered. counts_available says whether it did; a partial sum is
          indistinguishable from a complete one on the wire, so it is never
          presented as a total. */}
      <Field
        label="Mailboxes"
        tone={status.countsAvailable ? undefined : "warning"}
      >
        {status.countsAvailable ? (
          <>
            {status.mailboxes} across {status.domains}{" "}
            {plural(status.domains, "domain")}
          </>
        ) : (
          <>
            not reported · {status.domains} {plural(status.domains, "domain")}{" "}
            bound
          </>
        )}
      </Field>
      {status.missingPermissions.length ? (
        <Field label="Missing permissions" tone="warning">
          <span className="font-mono break-all">
            {status.missingPermissions.join(", ")}
          </span>
        </Field>
      ) : null}
      <Field label="API key">
        {engine?.reachable ? (
          <>
            configured · last verified{" "}
            {checkedAt ? <RelativeTime date={checkedAt} /> : "—"}
          </>
        ) : (
          <span className="text-warning">
            configured · last check failed
            {checkedAt ? (
              <>
                {" "}
                <RelativeTime date={checkedAt} />
              </>
            ) : null}
          </span>
        )}
      </Field>
      {/* delivery_stats_available means "counts exist", not "a secret is set":
          it is false for no receiver, for a receiver nothing has posted to yet,
          and for one that is refusing every delivery. This field therefore says
          only whether counts are being reported; the reason is the note below,
          which names which half of the wiring is missing. Naming a cause here
          would guess at one of three states (docs/mail-runbook.md). Both halves
          come from GetMailStatus; neither is inferred. */}
      <Field
        label="Delivery events"
        tone={status.deliveryStatsAvailable ? undefined : "warning"}
      >
        {status.deliveryStatsAvailable ? (
          <>
            counted from the mail server&rsquo;s events
            {status.deliveryWindowHours > 0 ? (
              <>
                {` · rolling ${status.deliveryWindowHours}-hour window`}
              </>
            ) : null}
          </>
        ) : (
          <>
            no counts reported
            {status.deliveryStatsNote ? (
              <span className="text-muted-foreground"> (see below)</span>
            ) : null}
          </>
        )}
      </Field>
      {/* A receiver that worked and then stopped keeps reporting counts and
          drifts to zero over the window. This is the field that shows it: the
          last delivery this control plane actually accepted. */}
      {lastDeliveryEvent ? (
        <Field label="Last delivery event">
          <RelativeTime date={lastDeliveryEvent} />
        </Field>
      ) : null}
      {status.deliveryStatsNote ? (
        <Field label="Delivery stats">{status.deliveryStatsNote}</Field>
      ) : null}
    </div>
  );
}

/* -------------------------------------------------------------------------- */
/* Billing                                                                     */
/* -------------------------------------------------------------------------- */

function BillingFields({ status }: { status: GetBillingStatusResponse }) {
  const reconciled = toDate(status.reconciledAt);
  const lastWebhook = toDate(status.lastWebhookAt);
  return (
    <div className="flex flex-col gap-2">
      <Field label="Provider">
        <span className="font-mono">{status.provider || "—"}</span>
      </Field>
      <Field label="Mode">{status.livemode ? "live" : "test"}</Field>
      <Field label="Price">{status.price?.label || "not reported"}</Field>
      <Field label="Webhook endpoint">
        {status.webhookConfigured ? "configured" : "not configured"}
      </Field>
      <Field label="Signing secrets">
        {status.webhookSecrets} {plural(status.webhookSecrets, "secret")}
      </Field>
      <Field
        label="Webhook queue"
        tone={status.deadWebhooks > 0 ? "warning" : undefined}
      >
        {status.pendingWebhooks} pending · {status.deadWebhooks} dead
        {lastWebhook ? (
          <>
            {" · last "}
            <RelativeTime date={lastWebhook} />
          </>
        ) : null}
      </Field>
      <Field label="Last reconcile">
        {reconciled ? <RelativeTime date={reconciled} /> : "—"}
      </Field>
      {status.policyNote ? (
        <Field label="Policy">{status.policyNote}</Field>
      ) : null}
    </div>
  );
}
