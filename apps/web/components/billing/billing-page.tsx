"use client";

import { useCallback, useState, type ReactNode } from "react";
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { Info } from "lucide-react";

import { EngineKind } from "@/gen/platform/v1/platform_pb";
import {
  SubscriptionState,
  type DomainBilling,
  type Invoice,
} from "@/gen/billing/v1/billing_pb";

import { EngineNotConfigured } from "@/components/app/engine-not-configured";
import { EngineUnreachable } from "@/components/app/engine-unreachable";
import { PageHeader } from "@/components/app/page-header";
import { usePlatform } from "@/components/app/platform-provider";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusPill } from "@/components/ui/status-pill";
import { useInvoices } from "@/hooks/use-platform-resources";
import { useNow } from "@/hooks/use-now";
import { getBillingClient } from "@/lib/platform-client";
import { describeError } from "@/lib/errors";
import { formatShortDate, plural, toDate } from "@/lib/format";
import {
  formatMoney,
  subscriptionLabel,
  subscriptionTone,
} from "@/lib/platform-model";
import { cn } from "@/lib/utils";

export type BillingPageProps = {
  /** `?canceled=` from the server component: the viewer came back from a cancelled checkout. */
  canceled?: string;
};

/**
 * Subscriptions, one per domain. Every number on this screen comes from
 * `GetBillingStatus` / `GetBillingSummary` / `ListInvoices`: the price is only ever
 * `status.price.label` (there is no build-time price anywhere in this app), the states
 * are the facade's, and nothing is gated on them — this release is informational, which
 * the policy note says in the control plane's own words.
 *
 * Three states before any of that (spec2 §8.1): the control API did not answer, the
 * engine slice has not loaded, or billing is not configured. Only the last one may say
 * "isn't configured".
 */
export function BillingPage({ canceled }: BillingPageProps) {
  const { billing, engine, status, statusError, refreshStatus } = usePlatform();

  return (
    <div className="flex flex-col gap-6">
      {statusError && !status ? (
        <>
          <PageHeader title="Billing" />
          {/* The control API did not answer: nothing is known about the engine, so the
              page says so instead of claiming it "isn't configured" (spec2 §8.1). */}
          <EngineUnreachable
            engine="billing"
            reason={statusError}
            onRetry={() => {
              void refreshStatus(true);
            }}
          />
        </>
      ) : !billing.loaded ? (
        <>
          <PageHeader title="Billing" />
          <PageSkeleton />
        </>
      ) : !billing.configured ? (
        <>
          <PageHeader title="Billing" />
          <EngineNotConfigured
            engine="billing"
            missingEnv={engine(EngineKind.BILLING)?.missingEnv ?? []}
          />
        </>
      ) : (
        <BillingBody canceled={canceled} />
      )}
    </div>
  );
}

function PageSkeleton() {
  return (
    <div
      data-slot="billing-skeleton"
      role="status"
      aria-busy="true"
      className="flex flex-col gap-3"
    >
      <span className="sr-only">Loading billing…</span>
      <Skeleton className="h-10 w-full" />
      <Skeleton className="h-10 w-full" />
      <Skeleton className="h-10 w-full" />
    </div>
  );
}

/* -------------------------------------------------------------------------- */
/* Links out                                                                   */
/* -------------------------------------------------------------------------- */

function parseUrl(raw: string): URL | undefined {
  if (!raw) {
    return undefined;
  }
  try {
    return new URL(raw);
  } catch {
    return undefined;
  }
}

/**
 * An invoice URL that may become a link. Stripe's hosted pages are https; the fake
 * provider serves them from the operator's own loopback interface, so http is allowed
 * there and nowhere else. Anything else is rendered as plain text — the value comes back
 * from an external API and must not be able to point the operator at an arbitrary
 * scheme or host.
 */
function externalUrl(raw: string, fakeProvider: boolean): string | undefined {
  const url = parseUrl(raw);
  if (!url) {
    return undefined;
  }
  if (url.protocol === "https:") {
    return url.toString();
  }
  const loopback =
    url.hostname === "127.0.0.1" ||
    url.hostname === "localhost" ||
    url.hostname === "[::1]";
  if (fakeProvider && url.protocol === "http:" && loopback) {
    return url.toString();
  }
  return undefined;
}

/**
 * A checkout or portal address the browser is sent to on an explicit click: https
 * anywhere, or plain http when this deployment runs the fake provider, whose pages are
 * served by the control plane itself.
 */
function redirectUrl(raw: string, fakeProvider: boolean): string | undefined {
  const url = parseUrl(raw);
  if (!url) {
    return undefined;
  }
  if (url.protocol === "https:" || (fakeProvider && url.protocol === "http:")) {
    return url.toString();
  }
  return undefined;
}

/* -------------------------------------------------------------------------- */
/* Body                                                                        */
/* -------------------------------------------------------------------------- */

function BillingBody({ canceled }: { canceled?: string }) {
  const { billing, invalidate } = usePlatform();
  const invoices = useInvoices();
  const now = useNow(60_000);

  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  // Set by the Retry button so the live region below keeps reporting the counts down
  // to "0 pending, 0 dead" — otherwise a queue that drains reads as silence.
  const [webhooksRetried, setWebhooksRetried] = useState(false);

  const status = billing.status;
  const summary = billing.summary;
  const fake = status?.provider === "fake";
  const rows = summary?.domains ?? [];

  const openPortal = useCallback(
    async (flow: string, zoneId: string, key: string) => {
      setBusy(key);
      setError(undefined);
      try {
        const response = await getBillingClient().createPortalSession({
          flow,
          zoneId,
          returnPath: "/billing",
        });
        const url = redirectUrl(response.url, fake);
        if (!url) {
          setError("The billing portal returned an address this console won't open.");
          return;
        }
        window.location.assign(url);
      } catch (caught) {
        setError(describeError(caught));
      } finally {
        setBusy(undefined);
      }
    },
    [fake],
  );

  const subscribe = useCallback(
    async (row: DomainBilling) => {
      setBusy(`subscribe:${row.zoneId}`);
      setError(undefined);
      try {
        const response = await getBillingClient().createCheckoutSession({
          zoneId: row.zoneId,
          returnPath: "/billing",
        });
        const url = redirectUrl(response.url, fake);
        if (!url) {
          setError("Checkout returned an address this console won't open.");
          return;
        }
        window.location.assign(url);
      } catch (caught) {
        setError(describeError(caught));
      } finally {
        setBusy(undefined);
      }
    },
    [fake],
  );

  const retryWebhooks = useCallback(async () => {
    setBusy("webhooks");
    setError(undefined);
    setWebhooksRetried(true);
    try {
      await getBillingClient().retryDeadWebhooks({});
      await invalidate();
    } catch (caught) {
      setError(describeError(caught));
    } finally {
      setBusy(undefined);
    }
  }, [invalidate]);

  return (
    <>
      <PageHeader
        title="Billing"
        subtitle={
          summary
            ? `${summary.activeCount} of ${rows.length} ${plural(
                rows.length,
                "domain",
              )} subscribed`
            : undefined
        }
        actions={
          <Button
            variant="outline"
            size="lg"
            className="text-ui rounded-[7px] px-3.5"
            disabled={busy !== undefined}
            onClick={() => {
              void openPortal("", "", "portal");
            }}
          >
            Manage billing
          </Button>
        }
      />

      {fake ? (
        <Alert role="status">
          <Info aria-hidden="true" className="text-primary" />
          <AlertTitle>Fake billing provider — no real charges.</AlertTitle>
        </Alert>
      ) : null}

      {canceled ? (
        <p className="text-ui text-muted-foreground">
          Checkout cancelled — nothing was charged.
        </p>
      ) : null}

      {status?.policyNote ? (
        <Alert role="status">
          <Info aria-hidden="true" className="text-muted-foreground" />
          <AlertDescription>
            <span className="text-soft">{status.policyNote}</span>
          </AlertDescription>
        </Alert>
      ) : null}

      {billing.error ? (
        <p role="alert" className="text-ui text-warning">
          Couldn&apos;t refresh billing: {billing.error}
        </p>
      ) : null}
      {error ? (
        <p role="alert" className="text-ui text-destructive">
          {error}
        </p>
      ) : null}

      <section aria-label="Plan" className="grid gap-4 md:grid-cols-2">
        <InfoCard label="Plan">
          {/* The only source of a price in this app. Nothing is substituted when the
              price probe has not succeeded. */}
          {status?.price?.label || "—"}
        </InfoCard>
        <InfoCard label="Payment method">
          {summary?.paymentMethod ? (
            <span className="flex flex-wrap items-baseline gap-2">
              <span>
                {summary.paymentMethod.brand} · {summary.paymentMethod.last4} ·{" "}
                {String(summary.paymentMethod.expMonth).padStart(2, "0")}/
                {String(summary.paymentMethod.expYear).slice(-2)}
              </span>
              <button
                type="button"
                className="link rounded-sm text-[13px] outline-none focus-visible:ring-2 focus-visible:ring-ring"
                disabled={busy !== undefined}
                onClick={() => {
                  void openPortal("payment_method_update", "", "payment-method");
                }}
              >
                Update
              </button>
            </span>
          ) : (
            "No card on file"
          )}
        </InfoCard>
      </section>

      {/* Retrying dead events changes these two counts through a refresh of the whole
          billing slice, and a subscription confirmed by a webhook changes the table
          below the same way. Mounted for the life of the page and rendered only once
          the slice has loaded, so the outcome of Retry — and a queue that fills up on
          its own — is announced instead of silently redrawn. */}
      <span role="status" className="sr-only">
        {status && (webhooksRetried || status.deadWebhooks > 0)
          ? `Billing webhooks: ${status.pendingWebhooks} pending, ${status.deadWebhooks} dead.`
          : ""}
      </span>

      {status && (status.pendingWebhooks > 0 || status.deadWebhooks > 0) ? (
        <p className="text-ui flex flex-wrap items-center gap-3 text-muted-foreground">
          <span>
            Webhooks: {status.pendingWebhooks} pending · {status.deadWebhooks} dead
          </span>
          {status.deadWebhooks > 0 ? (
            <Button
              variant="outline"
              size="sm"
              disabled={busy !== undefined}
              onClick={() => {
                void retryWebhooks();
              }}
            >
              Retry dead events
            </Button>
          ) : null}
        </p>
      ) : null}

      <DomainTable
        rows={rows}
        now={now}
        busy={busy}
        onSubscribe={subscribe}
        onCancel={(row) => {
          void openPortal(
            "subscription_cancel",
            row.zoneId,
            `cancel:${row.zoneId}`,
          );
        }}
      />

      <section aria-label="Invoices" className="flex flex-col gap-3">
        <h2 className="eyebrow">Invoices</h2>
        {!invoices.loaded ? (
          <Skeleton className="h-24 w-full" />
        ) : invoices.error ? (
          <p role="alert" className="text-ui text-warning">
            {invoices.error}
          </p>
        ) : !invoices.live ? (
          <p className="text-ui text-muted-foreground">
            Invoices couldn&apos;t be loaded from Stripe right now.
          </p>
        ) : invoices.items.length === 0 ? (
          <p className="text-ui text-muted-foreground">No invoices yet.</p>
        ) : (
          <>
            <InvoiceTable invoices={invoices.items} fake={fake} />
            {invoices.nextCursor ? (
              <div>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    void invoices.loadMore();
                  }}
                >
                  Load more
                </Button>
              </div>
            ) : null}
          </>
        )}
      </section>
    </>
  );
}

function InfoCard({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <div className="flex flex-col gap-1.5 rounded-[10px] border border-line px-5 py-[18px]">
      <div className="text-label text-muted-foreground">{label}</div>
      <div className="text-[15px] font-medium">{children}</div>
    </div>
  );
}

/* -------------------------------------------------------------------------- */
/* Subscriptions                                                               */
/* -------------------------------------------------------------------------- */

const domainColumns = "grid-cols-[1.4fr_140px_1fr_150px]";
const domainRowColumns = "md:grid-cols-[1.4fr_140px_1fr_150px]";

function renewsText(row: DomainBilling, now: Date): string {
  const end = toDate(row.currentPeriodEnd as Timestamp | undefined);
  const cancelAt = toDate(row.cancelAt as Timestamp | undefined);

  if (row.state === SubscriptionState.CANCELING) {
    const at = cancelAt ?? end;
    return at ? `Cancels ${formatShortDate(at)}` : "Cancels at the period end";
  }
  if (row.state === SubscriptionState.PENDING) {
    const expires = toDate(row.checkoutExpiresAt as Timestamp | undefined);
    if (!expires) {
      return "Checkout open";
    }
    const minutes = Math.round((expires.getTime() - now.getTime()) / 60_000);
    return minutes > 0
      ? `Checkout expires in ${minutes} ${plural(minutes, "min")}`
      : "Checkout expired";
  }
  // Only a subscription that will actually renew has a renewal date. A CANCELED
  // or UNBILLED row's `current_period_end` is history, not a promise, and
  // INCOMPLETE has not started a period yet.
  const renews =
    row.state === SubscriptionState.ACTIVE ||
    row.state === SubscriptionState.TRIALING ||
    row.state === SubscriptionState.PAST_DUE;
  return renews && end ? formatShortDate(end) : "—";
}

const subscribable = (state: SubscriptionState) =>
  state === SubscriptionState.UNBILLED ||
  state === SubscriptionState.CANCELED ||
  state === SubscriptionState.UNSPECIFIED;

const cancelable = (state: SubscriptionState) =>
  state === SubscriptionState.ACTIVE ||
  state === SubscriptionState.TRIALING ||
  state === SubscriptionState.PAST_DUE;

function DomainTable({
  rows,
  now,
  busy,
  onSubscribe,
  onCancel,
}: {
  rows: DomainBilling[];
  now: Date;
  busy?: string;
  onSubscribe: (row: DomainBilling) => void;
  onCancel: (row: DomainBilling) => void;
}) {
  if (!rows.length) {
    return (
      <div className="text-ui rounded-[10px] border border-line px-5 py-8 text-muted-foreground">
        No domains yet — connect one and it appears here.
      </div>
    );
  }

  return (
    <div
      role="table"
      aria-label="Subscriptions"
      className="overflow-hidden rounded-[10px] border border-line"
    >
      <div role="rowgroup">
        <div
          role="row"
          className={cn(
            "eyebrow hidden border-b border-line bg-fill-faint px-5 py-2.5 md:grid",
            domainColumns,
          )}
        >
          <div role="columnheader">Domain</div>
          <div role="columnheader">Status</div>
          <div role="columnheader">Renews</div>
          <div role="columnheader" className="text-right">
            Action
          </div>
        </div>
      </div>
      <div role="rowgroup">
        {rows.map((row) => {
          const tone = subscriptionTone(row.state);
          const label = subscriptionLabel(row.state);
          return (
            <div
              key={row.zoneId || row.zoneName}
              role="row"
              className={cn(
                "text-ui grid grid-cols-1 gap-1.5 border-b border-line-soft px-5 py-4 last:border-0 md:items-center md:gap-0",
                domainRowColumns,
              )}
            >
              <div role="cell" className="flex flex-col gap-0.5">
                <span className="font-medium break-all">{row.zoneName}</span>
                {row.zoneMissing ? (
                  <span className="text-xs text-warning">zone deleted</span>
                ) : null}
              </div>
              <div role="cell" className="flex gap-2.5 md:block">
                <span className="eyebrow w-16 shrink-0 pt-0.5 md:hidden">
                  Status
                </span>
                <StatusPill
                  tone={tone}
                  aria-label={label}
                  pulse={row.state === SubscriptionState.PENDING}
                >
                  {label}
                </StatusPill>
              </div>
              <div role="cell" className="flex gap-2.5 md:block">
                <span className="eyebrow w-16 shrink-0 pt-0.5 md:hidden">
                  Renews
                </span>
                <span className="font-mono text-[13px] text-muted-foreground">
                  {renewsText(row, now)}
                </span>
              </div>
              <div role="cell" className="md:text-right">
                {subscribable(row.state) && !row.zoneMissing ? (
                  <Button
                    size="sm"
                    disabled={busy !== undefined}
                    onClick={() => onSubscribe(row)}
                  >
                    Subscribe
                  </Button>
                ) : cancelable(row.state) ? (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy !== undefined}
                    onClick={() => onCancel(row)}
                  >
                    Cancel…
                  </Button>
                ) : null}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

/* -------------------------------------------------------------------------- */
/* Invoices                                                                    */
/* -------------------------------------------------------------------------- */

const invoiceColumns = "grid-cols-[1fr_110px_1.2fr_110px_110px_120px]";
const invoiceRowColumns = "md:grid-cols-[1fr_110px_1.2fr_110px_110px_120px]";

function InvoiceTable({
  invoices,
  fake,
}: {
  invoices: Invoice[];
  fake: boolean;
}) {
  return (
    <div
      role="table"
      aria-label="Invoices"
      className="overflow-hidden rounded-[10px] border border-line"
    >
      <div role="rowgroup">
        <div
          role="row"
          className={cn(
            "eyebrow hidden border-b border-line bg-fill-faint px-5 py-2.5 md:grid",
            invoiceColumns,
          )}
        >
          <div role="columnheader">Number</div>
          <div role="columnheader">Date</div>
          <div role="columnheader">Domain</div>
          <div role="columnheader">Total</div>
          <div role="columnheader">Status</div>
          <div role="columnheader" className="text-right">
            Links
          </div>
        </div>
      </div>
      <div role="rowgroup">
        {invoices.map((invoice) => {
          const created = toDate(invoice.createdAt as Timestamp | undefined);
          const hosted = externalUrl(invoice.hostedInvoiceUrl, fake);
          const pdf = externalUrl(invoice.invoicePdfUrl, fake);
          return (
            <div
              key={invoice.id}
              role="row"
              className={cn(
                "text-ui grid grid-cols-1 gap-1.5 border-b border-line-soft px-5 py-3.5 last:border-0 md:items-center md:gap-0",
                invoiceRowColumns,
              )}
            >
              <div role="cell" className="font-mono text-[13px] break-all">
                {invoice.number || invoice.id}
              </div>
              <div role="cell" className="text-muted-foreground">
                {created ? formatShortDate(created) : "—"}
              </div>
              <div role="cell" className="break-all">
                {invoice.zoneName || "—"}
              </div>
              <div role="cell" className="font-mono text-[13px]">
                {formatMoney(invoice.total, invoice.currency)}
              </div>
              <div role="cell" className="text-muted-foreground">
                {invoice.status || "—"}
              </div>
              <div
                role="cell"
                className="flex gap-3 text-[13px] md:justify-end"
              >
                {hosted ? (
                  <a
                    href={hosted}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="link"
                  >
                    View
                  </a>
                ) : (
                  <span className="text-muted-foreground">View</span>
                )}
                {pdf ? (
                  <a
                    href={pdf}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="link"
                  >
                    PDF
                  </a>
                ) : null}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
