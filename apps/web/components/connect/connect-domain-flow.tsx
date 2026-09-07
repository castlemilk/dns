"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useId, useState, type FormEvent, type ReactNode } from "react";
import { Code, ConnectError } from "@connectrpc/connect";

import { usePlatform } from "@/components/app/platform-provider";
import { useZones } from "@/components/app/zones-provider";
import { BillingStep } from "@/components/connect/billing-step";
import { EmailStep } from "@/components/connect/email-step";
import { NameserverList } from "@/components/connect/nameserver-list";
import { NameserverStatusBanner } from "@/components/connect/nameserver-status-banner";
import { RegistrarSteps } from "@/components/connect/registrar-steps";
import { StepRail } from "@/components/connect/step-rail";
import { WebsiteStep } from "@/components/connect/website-step";
import { DomainUnavailable } from "@/components/domain/domain-unavailable";
import { PointWebsiteForm } from "@/components/guided/point-website-form";
import { RouteEmailForm } from "@/components/guided/route-email-form";
import { ImportZoneDialog } from "@/components/zonefile/import-zone-dialog";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { useNameserverCheck } from "@/hooks/use-nameserver-check";
import { useNow } from "@/hooks/use-now";
import { connectHref, isHostname, normaliseZoneName, zoneHref } from "@/lib/dns-values";
import { describeError } from "@/lib/errors";

export type ConnectDomainFlowProps = {
  initialDomain?: string;
};

type Step = "domain" | "nameservers" | "website" | "email" | "billing";

const baseSteps: Array<{ key: Step; label: string }> = [
  { key: "domain", label: "Domain" },
  { key: "nameservers", label: "Point it here" },
  { key: "website", label: "Website" },
  { key: "email", label: "Email" },
];

const billingStep: { key: Step; label: string } = {
  key: "billing",
  label: "Billing",
};

const columnClassName =
  "mx-auto flex w-full max-w-[640px] flex-col gap-9 px-4 py-10 lg:py-[72px]";

export function ConnectDomainFlow({ initialDomain }: ConnectDomainFlowProps) {
  const router = useRouter();
  const ids = useId();
  const { loaded, loadedAt, getZone, createZone, refresh } = useZones();
  const {
    hosting,
    mail,
    billing,
    status: platformStatus,
    statusLoaded,
    statusError,
    refreshStatus,
  } = usePlatform();

  const [flow, setFlow] = useState<{ step?: Step; zoneName?: string; skipped: Step[] }>({
    skipped: [],
  });
  const [domainInput, setDomainInput] = useState(initialDomain ?? "");
  const [domainError, setDomainError] = useState<string>();
  const [alreadyHere, setAlreadyHere] = useState<string>();
  const [creating, setCreating] = useState(false);
  const [importOpen, setImportOpen] = useState(false);

  // Derived during render, never assigned from an effect: a zone that already exists
  // puts the flow straight on the nameservers step, so a reload of
  // /connect?domain=… does not flash step 1.
  const zone = getZone(flow.zoneName ?? initialDomain ?? "");
  const step: Step | undefined = !loaded
    ? undefined
    : zone
      ? flow.step && flow.step !== "domain"
        ? flow.step
        : "nameservers"
      : "domain";

  // The rail is built from what this deployment actually runs, and it is only rendered
  // once the platform status has settled — a rail that shows four steps and then grows a
  // fifth would tell the user the flow changed under them.
  const steps = billing.configured ? [...baseSteps, billingStep] : baseSteps;

  const onNameservers = step === "nameservers";
  const check = useNameserverCheck({
    domain: zone?.name ?? "",
    expected: zone?.nameservers ?? [],
    enabled: onNameservers && Boolean(zone),
    poll: true,
  });
  // A steady one-second clock: `useNow` only re-reads the time when its interval fires,
  // so pausing it on the other steps would leave a stale `now` on the way back here.
  const now = useNow(1000);
  const secondsAgo = check.lastCheckedAt
    ? Math.max(
        0,
        Math.round((now.getTime() - check.lastCheckedAt.getTime()) / 1000),
      )
    : undefined;

  function go(next: Step) {
    setFlow((current) => ({ ...current, step: next }));
  }

  function skip(from: Step, next: Step) {
    setFlow((current) => ({
      ...current,
      step: next,
      skipped: current.skipped.includes(from)
        ? current.skipped
        : [...current.skipped, from],
    }));
  }

  function finish() {
    if (zone) {
      router.replace(zoneHref(zone.name));
    }
  }

  // Email is the last step unless this deployment bills domains.
  function afterEmail() {
    if (billing.configured) {
      go("billing");
      return;
    }
    finish();
  }

  function skipEmail() {
    if (billing.configured) {
      skip("email", "billing");
      return;
    }
    finish();
  }

  async function handleCreate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const name = normaliseZoneName(domainInput);
    if (!isHostname(name)) {
      setAlreadyHere(undefined);
      setDomainError("Enter a bare domain such as studio.photos.");
      return;
    }
    if (getZone(name)) {
      setDomainError(undefined);
      setAlreadyHere(name);
      return;
    }

    setCreating(true);
    setDomainError(undefined);
    setAlreadyHere(undefined);
    try {
      const created = await createZone(name);
      // Keep the URL resumable: a reload lands on the nameservers step.
      window.history.replaceState(
        null,
        "",
        connectHref(created.name),
      );
      setFlow({ step: "nameservers", zoneName: created.name, skipped: [] });
    } catch (error) {
      if (ConnectError.from(error).code === Code.AlreadyExists) {
        setAlreadyHere(name);
      } else {
        setDomainError(describeError(error));
      }
    } finally {
      setCreating(false);
    }
  }

  async function continueExisting(name: string) {
    setAlreadyHere(undefined);
    // The zone may exist on the server but not in this session's list yet.
    if (!getZone(name)) {
      await refresh();
    }
    window.history.replaceState(
      null,
      "",
      connectHref(name),
    );
    setFlow({ step: "nameservers", zoneName: name, skipped: [] });
  }

  // `loaded` also becomes true when the first `listZones` failed, leaving no zones
  // at all. When the URL names a domain, step 1 would then offer to create a zone
  // that may well already exist, so say the list is unknown instead. Keyed on
  // `loadedAt` rather than `error`: dismissing the alert clears the error.
  const requested = flow.zoneName ?? initialDomain;
  if (step === "domain" && loadedAt === undefined && requested) {
    return (
      <div className={columnClassName}>
        <DomainUnavailable zoneName={requested} />
      </div>
    );
  }

  if (step === undefined || !statusLoaded) {
    return (
      <div className={columnClassName} role="status" aria-busy="true">
        <span className="sr-only">Loading the connect flow…</span>
        <Skeleton className="h-4 w-64" />
        <div className="flex flex-col gap-2.5">
          <Skeleton className="h-8 w-72" />
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-4/5" />
        </div>
        <Skeleton className="h-[120px] w-full rounded-xl" />
      </div>
    );
  }

  // `statusLoaded` also becomes true when `GetPlatformStatus` failed, and every
  // `configured` flag is then still its initial false. The rail silently loses the
  // Billing step and the Website/Email steps silently become the DNS-only forms, so
  // say which of the two it is rather than walking the operator through hand-written
  // records for engines this deployment may well run.
  const enginesUnknown = statusError !== undefined && platformStatus === undefined;

  return (
    <div className={columnClassName}>
      <StepRail steps={steps} current={step} skipped={flow.skipped} />

      {enginesUnknown ? (
        <Alert>
          <AlertTitle>
            The control plane didn&rsquo;t report which engines it runs
          </AlertTitle>
          <AlertDescription>
            <div className="text-soft">
              {statusError} Only the DNS steps are shown: if this deployment
              runs hosting, email or billing, those steps are missing because
              nothing could be asked, not because they are switched off.
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
      ) : null}

      {step === "domain" ? (
        <DomainStep
          ids={ids}
          value={domainInput}
          onChange={(next) => {
            setDomainInput(next);
            setDomainError(undefined);
            setAlreadyHere(undefined);
          }}
          error={domainError}
          alreadyHere={alreadyHere}
          creating={creating}
          onSubmit={handleCreate}
          onContinueExisting={continueExisting}
        />
      ) : null}

      {step !== "domain" && zone ? (
        <>
          {step === "nameservers" ? (
            <>
              <Heading
                title={`Point ${zone.name} to deep hosting`}
                paragraph={
                  <>
                    Change the domain&rsquo;s nameservers at your registrar to
                    the{" "}
                    {zone.nameservers.length === 2
                      ? "two"
                      : zone.nameservers.length}{" "}
                    below and this service answers for every record from then on.
                    It usually takes a few minutes to propagate, sometimes up to a
                    day — this page keeps checking.
                    {check.comparison.state === "elsewhere" ||
                    check.comparison.state === "partial" ? (
                      <>
                        {" "}
                        Your nameservers currently point at{" "}
                        {check.comparison.providerLabel ??
                          check.comparison.observed.join(", ")}
                        .
                      </>
                    ) : null}
                  </>
                }
              />

              <NameserverList nameservers={zone.nameservers} />
              <RegistrarSteps
                zoneName={zone.name}
                onImport={() => setImportOpen(true)}
              />
              <NameserverStatusBanner
                check={check}
                zoneName={zone.name}
                secondsAgo={secondsAgo}
                onSkip={() => skip("nameservers", "website")}
                onCheckNow={check.checkNow}
              />

              <Footer
                back={
                  <FooterBack onClick={() => router.push("/domains")}>
                    Back
                  </FooterBack>
                }
              >
                <Button
                  type="button"
                  variant="outline"
                  size="lg"
                  className="text-ui h-10 rounded-[7px] px-4"
                  onClick={() => skip("nameservers", "website")}
                >
                  I&rsquo;ll do this later
                </Button>
                <ContinueButton
                  enabled={check.comparison.state === "pointed"}
                  onClick={() => go("website")}
                />
              </Footer>

              {importOpen ? (
                <ImportZoneDialog
                  open={importOpen}
                  mode="replace"
                  zone={zone}
                  onOpenChange={setImportOpen}
                  onImported={() => setImportOpen(false)}
                />
              ) : null}
            </>
          ) : null}

          {step === "website" && hosting.configured ? (
            <>
              <WebsiteStep
                zone={zone}
                onContinue={() => go("email")}
                onSkip={() => skip("website", "email")}
              />
              <Footer
                back={
                  <FooterBack onClick={() => go("nameservers")}>Back</FooterBack>
                }
              />
            </>
          ) : null}

          {step === "website" && !hosting.configured ? (
            <>
              <Heading
                title={`Point ${zone.name} at a website`}
                paragraph={
                  <>
                    Where does {zone.name} live? Enter the address your host gave
                    you and the A, AAAA and www records are written for you.
                    {/* Only said when the control plane actually answered: a status
                        call that timed out knows nothing about the engines. */}
                    {platformStatus ? (
                      <span className="block text-muted-foreground">
                        Hosting isn&rsquo;t configured on this control plane.
                      </span>
                    ) : null}
                  </>
                }
              />
              <PointWebsiteForm
                zone={zone}
                layout="inline"
                submitLabel="Write records and continue"
                onApplied={() => go("email")}
                footerSlot={(submit) => (
                  <Footer
                    back={
                      <FooterBack onClick={() => go("nameservers")}>
                        Back
                      </FooterBack>
                    }
                  >
                    <Button
                      type="button"
                      variant="outline"
                      size="lg"
                      className="text-ui h-10 rounded-[7px] px-4"
                      onClick={() => skip("website", "email")}
                    >
                      Skip
                    </Button>
                    {submit}
                  </Footer>
                )}
              />
            </>
          ) : null}

          {step === "email" && mail.configured ? (
            <>
              <EmailStep zone={zone} onContinue={afterEmail} onSkip={skipEmail} />
              <Footer
                back={<FooterBack onClick={() => go("website")}>Back</FooterBack>}
              />
            </>
          ) : null}

          {step === "email" && !mail.configured ? (
            <>
              <Heading
                title={`Route email for ${zone.name}`}
                paragraph={
                  <>
                    Pick your mail provider. The MX, SPF and DMARC records it
                    asks for are written together.
                    {platformStatus ? (
                      <span className="block text-muted-foreground">
                        Mail isn&rsquo;t configured on this control plane.
                      </span>
                    ) : null}
                  </>
                }
              />
              <RouteEmailForm
                zone={zone}
                layout="inline"
                submitLabel={
                  billing.configured
                    ? "Write records and continue"
                    : "Write records and finish"
                }
                onApplied={afterEmail}
                footerSlot={(submit) => (
                  <Footer
                    back={
                      <FooterBack onClick={() => go("website")}>Back</FooterBack>
                    }
                  >
                    <Button
                      type="button"
                      variant="outline"
                      size="lg"
                      className="text-ui h-10 rounded-[7px] px-4"
                      onClick={skipEmail}
                    >
                      Skip
                    </Button>
                    {submit}
                  </Footer>
                )}
              />
            </>
          ) : null}

          {step === "billing" ? (
            <>
              <BillingStep zone={zone} onContinue={finish} onSkip={finish} />
              <Footer
                back={<FooterBack onClick={() => go("email")}>Back</FooterBack>}
              />
            </>
          ) : null}
        </>
      ) : null}
    </div>
  );
}

function Heading({
  title,
  paragraph,
}: {
  title: string;
  paragraph: ReactNode;
}) {
  return (
    <div className="flex flex-col gap-2.5">
      <h1 className="text-[30px] leading-[1.15] font-semibold break-words">
        {title}
      </h1>
      <p className="text-[15px] leading-[1.6] text-subtle">{paragraph}</p>
    </div>
  );
}

function Footer({
  back,
  children,
}: {
  back: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      {back}
      <div className="flex flex-wrap items-center gap-2.5">{children}</div>
    </div>
  );
}

function FooterBack({
  onClick,
  children,
}: {
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="text-ui rounded-sm text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
    >
      {children}
    </button>
  );
}

function ContinueButton({
  enabled,
  onClick,
}: {
  enabled: boolean;
  onClick: () => void;
}) {
  const tooltipId = useId();
  const className =
    "text-ui h-10 rounded-[7px] px-4 font-semibold disabled:bg-fill-strong disabled:text-faint disabled:opacity-100";

  if (enabled) {
    return (
      <Button type="button" size="lg" className={className} onClick={onClick}>
        Continue
      </Button>
    );
  }

  // A disabled Button is neither focusable nor hoverable (disabled:pointer-events-none),
  // so the tooltip hangs off a focusable wrapper instead.
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          tabIndex={0}
          className="inline-flex rounded-lg outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background"
          aria-describedby={tooltipId}
        >
          <Button type="button" size="lg" className={className} disabled>
            Continue
          </Button>
        </span>
      </TooltipTrigger>
      <TooltipContent id={tooltipId}>
        Enabled once the nameservers match
      </TooltipContent>
    </Tooltip>
  );
}

function DomainStep({
  ids,
  value,
  onChange,
  error,
  alreadyHere,
  creating,
  onSubmit,
  onContinueExisting,
}: {
  ids: string;
  value: string;
  onChange: (next: string) => void;
  error?: string;
  alreadyHere?: string;
  creating: boolean;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
  onContinueExisting: (name: string) => void;
}) {
  return (
    <>
      <Heading
        title="Connect a domain"
        paragraph="Enter the domain you own. We'll create its zone here, then walk you through pointing it at these nameservers, your website and your email."
      />
      <form onSubmit={onSubmit} className="flex flex-col gap-5">
        <div className="grid gap-2">
          <Label htmlFor="connect-domain">Domain</Label>
          <Input
            id="connect-domain"
            name="domain"
            className="h-10 font-mono"
            autoComplete="off"
            autoCapitalize="none"
            autoFocus
            spellCheck={false}
            inputMode="url"
            placeholder="studio.photos"
            value={value}
            onChange={(event) => onChange(event.target.value)}
            aria-invalid={error ? true : undefined}
            aria-describedby={error ? `${ids}-domain-error` : undefined}
            disabled={creating}
          />
          {error ? (
            <p
              id={`${ids}-domain-error`}
              role="alert"
              className="text-xs text-destructive"
            >
              {error}
            </p>
          ) : null}
        </div>

        {alreadyHere ? (
          <Alert>
            <AlertTitle>{alreadyHere} is already here.</AlertTitle>
            <AlertDescription className="flex flex-col items-start gap-2.5">
              <span>Its zone exists, so there is nothing to create.</span>
              <Button
                type="button"
                size="sm"
                onClick={() => onContinueExisting(alreadyHere)}
              >
                Continue its setup
              </Button>
            </AlertDescription>
          </Alert>
        ) : null}

        <Footer
          back={
            <Link
              href="/domains"
              className="text-ui rounded-sm text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
            >
              Cancel
            </Link>
          }
        >
          <Button
            type="submit"
            size="lg"
            className="text-ui h-10 rounded-[7px] px-4 font-semibold"
            disabled={creating || !value.trim()}
          >
            {creating ? "Creating zone…" : "Create zone and continue"}
          </Button>
        </Footer>
      </form>
    </>
  );
}
