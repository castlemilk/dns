"use client";

import { useState } from "react";

import { usePlatform } from "@/components/app/platform-provider";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import type { Zone } from "@/gen/dns/v1/dns_pb";
import { WwwMode, type Site } from "@/gen/hosting/v1/hosting_pb";
import { describeError } from "@/lib/errors";
import { getHostingClient } from "@/lib/platform-client";
import { wwwHostname, wwwModeReading } from "@/lib/platform-model";

export type WwwModeDialogProps = {
  zone: Zone;
  site: Site;
  /** `capabilities.domain_redirect` on the hosting engine status. */
  domainRedirectSupported: boolean;
  open: boolean;
  onOpenChange(open: boolean): void;
  onDone?(): void;
};

type Choice = "serve" | "redirect";

const choiceOf = (site: Site): Choice =>
  site.wwwMode === WwwMode.REDIRECT ? "redirect" : "serve";

/**
 * What `www.<zone>` does. The DNS records are the same either way — the www
 * alias still points at the apex, because the 301 happens at the hosting
 * engine's gateway and nothing reaches the gateway without the record.
 *
 * `UpdateSiteRequest.www_mode` is UNSPECIFIED for "leave it alone", so this
 * dialog only ever sends a mode when the operator changed one, and a redirect
 * is offered only when the site actually serves a www host.
 */
export function WwwModeDialog({
  zone,
  site,
  domainRedirectSupported,
  open,
  onOpenChange,
  onDone,
}: WwwModeDialogProps) {
  const { refreshSite } = usePlatform();
  const [choice, setChoice] = useState<Choice>(() => choiceOf(site));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [ignored, setIgnored] = useState(false);

  const reading = wwwModeReading(zone.name, site, domainRedirectSupported);
  const changed = choice !== choiceOf(site);

  async function handleSubmit() {
    setError(undefined);
    setIgnored(false);
    setBusy(true);
    try {
      const response = await getHostingClient().updateSite({
        zoneId: zone.id,
        wwwMode: choice === "redirect" ? WwwMode.REDIRECT : WwwMode.SERVE,
      });
      // The response reports what the engine holds, not what was asked for: an
      // engine that ignores the field answers with no redirect, and saying so
      // is the only honest reading of a request that appeared to succeed.
      const updated = response.site;
      const reported = wwwHostname(updated)?.redirectTo.trim() ?? "";
      await refreshSite(zone.id);
      if (choice === "redirect" && reported === "") {
        setIgnored(true);
        setBusy(false);
        return;
      }
      onDone?.();
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
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>www.{zone.name}</DialogTitle>
          <DialogDescription>
            Both hosts are registered with the hosting engine and both keep
            their DNS records; this only changes what the engine answers on the
            www host.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-3">
          <p className="text-ui text-muted-foreground">
            Now: {reading.text}
            {reading.note ? ` — ${reading.note}` : ""}
          </p>

          {site.www ? (
            <RadioGroup
              value={choice}
              onValueChange={(next) => setChoice(next as Choice)}
              disabled={busy}
              aria-label={`What www.${zone.name} does`}
            >
              <div className="flex items-start gap-2">
                <RadioGroupItem
                  id="www-mode-serve"
                  value="serve"
                  className="mt-0.5"
                />
                <Label htmlFor="www-mode-serve" className="font-normal">
                  Serves the same site
                </Label>
              </div>
              <div className="flex items-start gap-2">
                <RadioGroupItem
                  id="www-mode-redirect"
                  value="redirect"
                  className="mt-0.5"
                />
                <Label htmlFor="www-mode-redirect" className="font-normal">
                  Redirects to {zone.name} (301)
                </Label>
              </div>
            </RadioGroup>
          ) : (
            <p className="text-xs text-warning">
              This site doesn&apos;t serve www.{zone.name}, so there is nothing
              to redirect. Attach it with the www host to choose.
            </p>
          )}

          {site.www && !domainRedirectSupported ? (
            <p className="text-xs text-muted-foreground">
              This hosting engine has never reported a host redirect. If its
              version has no redirect field, www keeps serving the same site and
              the row above will say so.
            </p>
          ) : null}

          {ignored ? (
            <p role="alert" className="text-xs text-warning">
              The setting was saved, but the hosting engine reports no redirect
              on www.{zone.name}, so www still serves the same site — this
              engine version probably cannot redirect a host. Set it back to
              Serve to clear the request.
            </p>
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
            {ignored ? "Close" : "Cancel"}
          </Button>
          <Button
            onClick={() => {
              void handleSubmit();
            }}
            disabled={busy || !changed || !site.www}
          >
            {busy ? "Saving…" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
