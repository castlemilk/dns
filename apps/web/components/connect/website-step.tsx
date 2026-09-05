"use client";

import { useId, useState, type FormEvent } from "react";

import { RecordChangeList } from "@/components/app/record-change-list";
import { usePlatform, useSite } from "@/components/app/platform-provider";
import { DeployProgress } from "@/components/connect/deploy-progress";
import { PointWebsiteForm } from "@/components/guided/point-website-form";
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
import { StatusPill } from "@/components/ui/status-pill";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import {
  DeployPhase,
  Framework,
  SiteState,
} from "@/gen/hosting/v1/hosting_pb";
import type { RecordChange } from "@/gen/platform/v1/platform_pb";
import { useDeployPolling } from "@/hooks/use-deploy-polling";
import { describeError } from "@/lib/errors";
import { getHostingClient } from "@/lib/platform-client";
import { siteStateLabel, siteStateTone } from "@/lib/platform-model";
import { plural } from "@/lib/format";

export type WebsiteStepProps = {
  zone: Zone;
  onContinue(): void;
  onSkip(): void;
};

const repositoryPattern =
  /^https:\/\/github\.com\/[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+(\.git)?$/;

const frameworks: Array<{ value: string; label: string }> = [
  { value: "static", label: "Static site" },
  { value: "node", label: "Node" },
  { value: "nextjs", label: "Next.js" },
];

const frameworkValues: Record<string, Framework> = {
  static: Framework.STATIC,
  node: Framework.NODE,
  nextjs: Framework.NEXTJS,
};

/**
 * The connect flow's Website step when the hosting engine is configured: attach the
 * domain to the engine and, optionally, start the first deploy in the same submit.
 *
 * The per-deploy token lives in this component's state for exactly as long as the
 * `CreateDeploy` call takes — it is cleared as soon as the promise settles, is never
 * prefilled, and is never sent anywhere but the control plane's own facade.
 */
export function WebsiteStep({ zone, onContinue, onSkip }: WebsiteStepProps) {
  const ids = useId();
  const { hosting, invalidate, refreshSite } = usePlatform();
  const { site } = useSite(zone.id);
  useDeployPolling(site);

  const [mode, setMode] = useState<"here" | "elsewhere">("here");
  const [framework, setFramework] = useState("static");
  const [repository, setRepository] = useState("");
  const [branch, setBranch] = useState("main");
  const [www, setWww] = useState(true);
  const [token, setToken] = useState("");
  const [tokenOpen, setTokenOpen] = useState(false);
  const [replace, setReplace] = useState(false);
  const [conflicts, setConflicts] = useState<RecordChange[]>([]);
  const [plan, setPlan] = useState<RecordChange[]>([]);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  const uploadsEnabled = hosting.status?.uploadsEnabled ?? false;
  const latest = site?.latest;
  // Continue as soon as the build is actually running (or already finished): a queued
  // build has not been picked up by the engine yet, so there is nothing to watch on the
  // domain page. Nothing about the outcome is claimed either way.
  const continueEnabled =
    site !== undefined &&
    (latest === undefined || latest.phase !== DeployPhase.QUEUED);

  async function attachAndDeploy(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(undefined);

    const repo = repository.trim();
    if (repo && !repositoryPattern.test(repo)) {
      setError("Enter a repository URL like https://github.com/owner/name.");
      return;
    }

    setBusy(true);
    try {
      const client = getHostingClient();
      const request = {
        zoneId: zone.id,
        framework: frameworkValues[framework] ?? Framework.STATIC,
        repository: repo,
        branch: branch.trim() || "main",
        path: "",
        skipWww: !www,
        replaceConflictingRecords: replace,
      };

      // Ask for the plan first so a domain that already has custom website records
      // shows exactly what would change before anything is written.
      const preview = await client.attachSite({ ...request, dryRun: true });
      setPlan(preview.dnsPlan);
      setConflicts(preview.conflicts);
      if (preview.conflicts.length && !replace) {
        setBusy(false);
        return;
      }

      await client.attachSite({ ...request, dryRun: false });
      await invalidate();

      if (repo) {
        try {
          await client.createDeploy({
            zoneId: zone.id,
            repository: repo,
            revision: branch.trim() || "main",
            path: "",
            gitToken: token,
            framework: frameworkValues[framework] ?? Framework.STATIC,
            uploadId: "",
          });
        } finally {
          // The token never outlives the request it was pasted for.
          setToken("");
          setTokenOpen(false);
        }
        await refreshSite(zone.id);
      }
    } catch (caught) {
      setError(describeError(caught));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-2.5">
        <h1 className="text-[30px] leading-[1.15] font-semibold break-words">
          Put {zone.name} online
        </h1>
        <p className="text-[15px] leading-[1.6] text-subtle">
          {/* The www half is the checkbox below, so this sentence reads it rather
              than promising a host the operator may have just cleared. */}
          Connect a public GitHub repository. The site is built and served at{" "}
          {zone.name}
          {www ? ` and www.${zone.name}` : ""}; a certificate is issued
          automatically once the nameservers point here, and the DNS records are
          written for you. Private repository? Paste a token for this deploy
          only.
          {uploadsEnabled
            ? " You can also upload a folder from the domain page once the site is attached."
            : ""}
        </p>
      </div>

      {site ? (
        <div className="flex flex-col gap-4">
          <div className="flex flex-wrap items-center gap-2.5">
            <StatusPill
              tone={siteStateTone(site.state)}
              pulse={
                site.state === SiteState.ATTACHING ||
                site.state === SiteState.DETACHING
              }
              aria-label={siteStateLabel(site.state)}
            >
              {siteStateLabel(site.state)}
            </StatusPill>
            <span className="text-ui text-muted-foreground">
              {site.state === SiteState.ATTACHING
                ? `Registering ${zone.name} and www.${zone.name} with the hosting engine…`
                : site.reason || `${zone.name} is attached to the hosting engine.`}
            </span>
          </div>
          {latest ? <DeployProgress deploy={latest} /> : null}
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
              type="button"
              size="lg"
              className="text-ui h-10 rounded-[7px] px-4 font-semibold"
              disabled={!continueEnabled}
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
              <Label htmlFor={`${ids}-here`} className="flex flex-col items-start gap-1">
                <span className="text-ui font-medium">Host it here</span>
                <span className="text-[13px] font-normal text-muted-foreground">
                  Build and serve the site on this platform&rsquo;s hosting engine.
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
                <span className="text-ui font-medium">Point at another host</span>
                <span className="text-[13px] font-normal text-muted-foreground">
                  Write A, AAAA or CNAME records for a site that lives somewhere else.
                </span>
              </Label>
            </div>
          </RadioGroup>

          {mode === "here" ? (
            <form onSubmit={attachAndDeploy} className="flex flex-col gap-5">
              <div className="grid gap-2">
                <Label htmlFor={`${ids}-framework`}>Framework</Label>
                <Select value={framework} onValueChange={setFramework}>
                  <SelectTrigger id={`${ids}-framework`} className="h-10 w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {frameworks.map((entry) => (
                      <SelectItem key={entry.value} value={entry.value}>
                        {entry.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {framework === "nextjs" ? (
                  <p className="text-xs text-muted-foreground">
                    Next.js needs <span className="font-mono">output: standalone</span>.
                  </p>
                ) : null}
              </div>

              <div className="grid gap-2">
                <Label htmlFor={`${ids}-repository`}>
                  GitHub repository{" "}
                  <span className="text-muted-foreground">(optional)</span>
                </Label>
                <Input
                  id={`${ids}-repository`}
                  className="h-10 font-mono"
                  autoComplete="off"
                  spellCheck={false}
                  inputMode="url"
                  placeholder="https://github.com/owner/name"
                  value={repository}
                  onChange={(event) => setRepository(event.target.value)}
                />
                <p className="text-xs text-muted-foreground">
                  Leave it empty to attach the domain now and deploy later.
                </p>
              </div>

              <div className="grid gap-2">
                <Label htmlFor={`${ids}-branch`}>Branch</Label>
                <Input
                  id={`${ids}-branch`}
                  className="h-10 font-mono"
                  autoComplete="off"
                  spellCheck={false}
                  value={branch}
                  onChange={(event) => setBranch(event.target.value)}
                />
              </div>

              <div className="flex items-center gap-2.5">
                <Checkbox
                  id={`${ids}-www`}
                  checked={www}
                  onCheckedChange={(checked) => setWww(checked === true)}
                />
                <Label htmlFor={`${ids}-www`} className="font-normal">
                  Also serve www.{zone.name}
                </Label>
              </div>

              <details
                open={tokenOpen}
                onToggle={(event) =>
                  setTokenOpen((event.currentTarget as HTMLDetailsElement).open)
                }
                className="rounded-[10px] border border-line px-4 py-3"
              >
                <summary className="text-ui cursor-pointer select-none">
                  Private repository
                </summary>
                <div className="mt-3 grid gap-2">
                  <Label htmlFor={`${ids}-token`}>Access token</Label>
                  <Input
                    id={`${ids}-token`}
                    type="password"
                    autoComplete="off"
                    className="h-10 font-mono"
                    value={token}
                    onChange={(event) => setToken(event.target.value)}
                  />
                  <p className="text-xs text-muted-foreground">
                    Used once for this build, sent straight to the hosting engine
                    and deleted after it; never stored here.
                  </p>
                </div>
              </details>

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
                  {busy
                    ? "Working…"
                    : repository.trim()
                      ? "Attach and deploy"
                      : "Attach site"}
                </Button>
              </div>
            </form>
          ) : (
            <PointWebsiteForm
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
    </div>
  );
}
