"use client";

import { useCallback, useState } from "react";

import { usePlatform } from "@/components/app/platform-provider";
import { RecordChangeList } from "@/components/app/record-change-list";
import { useRecordPreview } from "@/components/hosting/use-record-preview";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
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
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import {
  Framework,
  WwwMode,
  type AttachSiteResponse,
  type GetHostingStatusResponse,
} from "@/gen/hosting/v1/hosting_pb";
import { describeError } from "@/lib/errors";
import { plural } from "@/lib/format";
import { getHostingClient } from "@/lib/platform-client";

export type AttachSiteDialogProps = {
  zone: Zone;
  hostingStatus: GetHostingStatusResponse;
  open: boolean;
  onOpenChange(open: boolean): void;
  onDone?(result: AttachSiteResponse): void;
};

const frameworks = [
  { value: String(Framework.STATIC), label: "Static site" },
  { value: String(Framework.NODE), label: "Node" },
  { value: String(Framework.NEXTJS), label: "Next.js" },
] as const;

const repositoryPattern =
  /^https:\/\/github\.com\/[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+(\.git)?$/;

/**
 * Attaching a site writes the apex A/AAAA records and the `www` alias, so the dialog
 * shows the exact plan the control plane computed with `dry_run: true` before anything
 * is written, and refuses to submit while conflicting custom records are unresolved.
 */
export function AttachSiteDialog({
  zone,
  hostingStatus,
  open,
  onOpenChange,
  onDone,
}: AttachSiteDialogProps) {
  const { invalidate } = usePlatform();
  const [framework, setFramework] = useState<string>(String(Framework.STATIC));
  const [serveWww, setServeWww] = useState(true);
  const [wwwMode, setWwwMode] = useState<"serve" | "redirect">("serve");
  const [repository, setRepository] = useState("");
  const [branch, setBranch] = useState("");
  const [path, setPath] = useState("");
  const [replace, setReplace] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const frameworkValue = Number(framework) as Framework;
  const skipWww = !serveWww;

  // Only the fields the plan depends on: the repository and branch are app metadata and
  // never change a single DNS record, so typing them must not re-ask the control plane.
  const runPreview = useCallback(
    async (signal: AbortSignal) => {
      const response = await getHostingClient().attachSite(
        {
          zoneId: zone.id,
          framework: frameworkValue,
          skipWww,
          dryRun: true,
        },
        { signal },
      );
      return { dnsPlan: response.dnsPlan, conflicts: response.conflicts };
    },
    [frameworkValue, skipWww, zone.id],
  );

  const preview = useRecordPreview(
    open,
    `${frameworkValue}:${skipWww}`,
    runPreview,
  );

  const repositoryInvalid =
    repository.trim().length > 0 && !repositoryPattern.test(repository.trim());
  const conflicts = preview.conflicts.length;
  const blocked = conflicts > 0 && !replace;

  async function handleSubmit() {
    setError(undefined);
    setBusy(true);
    try {
      const result = await getHostingClient().attachSite({
        zoneId: zone.id,
        framework: frameworkValue,
        repository: repository.trim(),
        branch: branch.trim(),
        path: path.trim(),
        skipWww,
        // REDIRECT is refused when there is no www host, so it is only ever
        // sent with one; SERVE is what every site did before the field existed.
        wwwMode:
          serveWww && wwwMode === "redirect" ? WwwMode.REDIRECT : WwwMode.SERVE,
        replaceConflictingRecords: replace,
        dryRun: false,
      });
      await invalidate();
      onDone?.(result);
      onOpenChange(false);
    } catch (caught) {
      setError(describeError(caught));
      setBusy(false);
    }
  }

  if (!open) {
    return null;
  }

  return (
    <Dialog
      open
      onOpenChange={(next) => {
        if (!next && busy) {
          return;
        }
        onOpenChange(next);
      }}
    >
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Host {zone.name} here</DialogTitle>
          <DialogDescription>
            The site is built by the hosting engine and served at {zone.name}
            {serveWww
              ? wwwMode === "redirect"
                ? `, with www.${zone.name} redirecting to it`
                : ` and www.${zone.name}`
              : ""}
            . The records below are written into this zone and kept up to date
            automatically.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-3">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="attach-framework">Framework</Label>
            <Select
              value={framework}
              onValueChange={setFramework}
              disabled={busy}
            >
              <SelectTrigger id="attach-framework" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {frameworks.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {frameworkValue === Framework.NEXTJS ? (
              <p className="text-xs text-muted-foreground">
                Next.js needs <code className="font-mono">output: standalone</code>
                .
              </p>
            ) : null}
          </div>

          <div className="flex flex-col gap-2">
            <div className="flex items-center gap-2">
              <Checkbox
                id="attach-www"
                checked={serveWww}
                onCheckedChange={(checked) => setServeWww(checked === true)}
                disabled={busy}
              />
              <Label htmlFor="attach-www" className="font-normal">
                Register www.{zone.name} as well
              </Label>
            </div>
            {/* The www record is written either way: the 301 happens at the
                hosting engine's gateway, which nothing reaches without it. */}
            {serveWww ? (
              <RadioGroup
                value={wwwMode}
                onValueChange={(next) =>
                  setWwwMode(next === "redirect" ? "redirect" : "serve")
                }
                disabled={busy}
                aria-label={`What www.${zone.name} does`}
                className="pl-6"
              >
                <div className="flex items-start gap-2">
                  <RadioGroupItem
                    id="attach-www-serve"
                    value="serve"
                    className="mt-0.5"
                  />
                  <Label htmlFor="attach-www-serve" className="font-normal">
                    Serves the same site
                  </Label>
                </div>
                <div className="flex items-start gap-2">
                  <RadioGroupItem
                    id="attach-www-redirect"
                    value="redirect"
                    className="mt-0.5"
                  />
                  <Label htmlFor="attach-www-redirect" className="font-normal">
                    Redirects to {zone.name} (301)
                  </Label>
                </div>
              </RadioGroup>
            ) : null}
          </div>

          <div className="flex flex-col gap-1.5">
            <Label htmlFor="attach-repository">
              Repository{" "}
              <span className="font-normal text-muted-foreground">
                (optional)
              </span>
            </Label>
            <Input
              id="attach-repository"
              value={repository}
              onChange={(event) => setRepository(event.target.value)}
              placeholder="https://github.com/owner/name"
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              disabled={busy}
              aria-invalid={repositoryInvalid || undefined}
              className="font-mono text-[13px]"
            />
            <p className="text-xs text-muted-foreground">
              Remembered for the next deploy. You can add it later.
            </p>
            {repositoryInvalid ? (
              <p role="alert" className="text-xs text-destructive">
                Use the https://github.com/owner/name URL.
              </p>
            ) : null}
          </div>

          <div className="grid gap-3 sm:grid-cols-2">
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="attach-branch">Branch</Label>
              <Input
                id="attach-branch"
                value={branch}
                onChange={(event) => setBranch(event.target.value)}
                placeholder="main"
                autoComplete="off"
                spellCheck={false}
                disabled={busy}
                className="font-mono text-[13px]"
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="attach-path">Directory</Label>
              <Input
                id="attach-path"
                value={path}
                onChange={(event) => setPath(event.target.value)}
                placeholder="/"
                autoComplete="off"
                spellCheck={false}
                disabled={busy}
                className="font-mono text-[13px]"
              />
            </div>
          </div>

          <div className="flex flex-col gap-2 rounded-[10px] border border-line bg-fill-faint px-3.5 py-3">
            <p className="eyebrow">Records to write</p>
            {preview.loading && !preview.loaded ? (
              <div className="flex flex-col gap-1.5">
                <Skeleton className="h-4 w-3/4" />
                <Skeleton className="h-4 w-2/3" />
              </div>
            ) : preview.error ? (
              <p role="alert" className="text-xs text-destructive">
                {preview.error}
              </p>
            ) : (
              <RecordChangeList
                changes={preview.changes}
                conflicts={preview.conflicts}
              />
            )}
            {hostingStatus.gatewayAddresses.length ? (
              <p className="text-xs text-muted-foreground">
                Gateway {plural(hostingStatus.gatewayAddresses.length, "address")}
                :{" "}
                <span className="font-mono">
                  {hostingStatus.gatewayAddresses.join(", ")}
                </span>
              </p>
            ) : null}
          </div>

          {conflicts > 0 ? (
            <div className="flex items-start gap-2">
              <Checkbox
                id="attach-replace"
                checked={replace}
                onCheckedChange={(checked) => setReplace(checked === true)}
                disabled={busy}
                className="mt-0.5"
              />
              <Label htmlFor="attach-replace" className="font-normal text-warning">
                Replace the {conflicts} custom {plural(conflicts, "record")}{" "}
                listed above
              </Label>
            </div>
          ) : null}

          {error ? (
            <p role="alert" className="text-xs text-destructive">
              {error}
            </p>
          ) : null}
        </div>

        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={busy}
          >
            Cancel
          </Button>
          <Button
            onClick={() => {
              void handleSubmit();
            }}
            disabled={busy || blocked || repositoryInvalid}
          >
            {busy ? "Attaching…" : "Attach and write records"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
