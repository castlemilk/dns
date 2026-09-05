"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useState, type ReactNode } from "react";
import { LoaderCircle, SquareTerminal, TriangleAlert } from "lucide-react";

import { CliCode, CliPanel } from "@/components/cli/cli-panel";
import {
  parseCallbackRequest,
  postApproval,
  postDenial,
  type CallbackQuery,
  type CallbackRequest,
} from "@/components/cli/callback-request";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { getOperatorToken } from "@/lib/operator-session";
import { getApiBaseUrl } from "@/lib/operator-transport";

/**
 * The approval screen `simple auth login` opens, rendered inside the console's
 * own shell so it is only reachable once the operator gate is unlocked.
 *
 * It is deliberately blunt about what approving does. There is no identity
 * provider here and no per-CLI scope: approving copies the single operator
 * token this browser tab is holding to a program listening on this machine's
 * loopback address. The screen says that, names the control API the token is
 * for, names the port it will be posted to, and admits the one thing the
 * console cannot check — which program is on the other end of that port.
 *
 * The token only ever moves in the body of that POST. It is not put in a link,
 * a redirect, a log line or this page's address bar.
 */

type Phase = "idle" | "approving" | "denying" | "failed";

export function CliAuthorize(query: CallbackQuery) {
  const router = useRouter();
  const [phase, setPhase] = useState<Phase>("idle");
  const [failure, setFailure] = useState<string>();
  const parsed = parseCallbackRequest(query);

  if (!parsed.ok) {
    return <InvalidLink problem={parsed.problem} />;
  }

  const request = parsed.request;
  const apiBaseUrl = getApiBaseUrl();
  const busy = phase === "approving" || phase === "denying";

  async function approve(target: CallbackRequest) {
    setFailure(undefined);
    const token = getOperatorToken();
    if (!token) {
      setPhase("failed");
      setFailure(
        "This tab is no longer holding an operator token. Unlock the console again, then re-run `simple auth login`.",
      );
      return;
    }

    setPhase("approving");
    try {
      await postApproval(target, token);
    } catch (error) {
      setPhase("failed");
      setFailure(
        error instanceof Error
          ? error.message
          : "The token could not be handed over.",
      );
      return;
    }

    // Stays "approving" through the navigation: the buttons must not come back
    // to life on a screen whose token has already been handed over.
    router.replace("/cli/approved");
  }

  async function deny(target: CallbackRequest) {
    setFailure(undefined);
    setPhase("denying");
    // Telling the CLI now means it exits immediately instead of waiting out its
    // five-minute timeout. It cannot fail in a way that matters: nothing was
    // handed over, which is exactly what the refusal page says.
    await postDenial(target);
    router.replace("/cli/denied");
  }

  return (
    <CliPanel
      tone="info"
      icon={<SquareTerminal className="size-[18px]" aria-hidden="true" />}
      title="Hand this console's operator token to the simple CLI?"
      lead={
        <>
          Something running <CliCode>simple auth login</CliCode> on this machine
          opened this page and is waiting on a loopback port for an answer.
        </>
      }
    >
      <dl className="mt-6 grid gap-3 border-t border-line-soft pt-5">
        <Detail label="What it receives">
          The operator token this browser tab is holding — the same one, in full.
        </Detail>
        <Detail label="Which API it is for">
          <CliCode>{apiBaseUrl || "this console's own origin"}</CliCode>
        </Detail>
        <Detail label="Where it is posted">
          <CliCode>{request.callbackUrl}</CliCode> — loopback on this machine,
          in a request body, never in a link.
        </Detail>
      </dl>

      <h2 className="text-ui mt-6 font-semibold">
        What the CLI will be able to do
      </h2>
      <ul className="text-ui mt-2 flex list-disc flex-col gap-1.5 pl-5 leading-[1.55] text-subtle">
        <li>Read, add, change and delete every zone and record.</li>
        <li>
          Attach and detach websites, deploy, roll back, and read or set their
          environment variables.
        </li>
        <li>
          Bind and unbind email, and create, reset and delete mailboxes and
          forwarders.
        </li>
        <li>Read billing, start a subscription and open the billing portal.</li>
        <li>Read the activity log.</li>
      </ul>
      <p className="text-ui mt-2 leading-[1.55] text-subtle">
        That is everything this console can do, because it is the same token.
        There is no smaller scope to grant.
      </p>

      <div className="mt-6 flex flex-col gap-2 rounded-lg border border-line-soft bg-fill-faint px-4 py-3.5 text-[13px] leading-[1.55] text-muted-foreground">
        <p className="flex items-start gap-2">
          <TriangleAlert
            className="mt-0.5 size-3.5 shrink-0 text-warning"
            aria-hidden="true"
          />
          <span>
            The console cannot check which program is listening on port{" "}
            {request.port}. Approve only if you just ran{" "}
            <CliCode>simple auth login</CliCode> yourself.
          </span>
        </p>
        <p>
          This is not a sign-in. There is no identity provider and no user
          account — approving copies a shared credential.
        </p>
        <p>
          The CLI writes it to{" "}
          <CliCode>~/.config/simple/config.json</CliCode> with{" "}
          <CliCode>0600</CliCode> permissions and keeps it until you run{" "}
          <CliCode>simple auth logout</CliCode>. Revoking it everywhere means
          rotating <CliCode>DNS_API_BEARER_TOKEN</CliCode> on the control plane;
          there is no per-CLI revocation.
        </p>
      </div>

      {failure ? (
        <Alert variant="destructive" className="mt-5">
          <TriangleAlert aria-hidden="true" />
          <AlertTitle>The CLI wasn&rsquo;t reachable</AlertTitle>
          <AlertDescription>{failure}</AlertDescription>
        </Alert>
      ) : null}

      <div className="mt-6 flex flex-wrap items-center gap-2.5">
        <Button
          type="button"
          size="lg"
          disabled={busy}
          onClick={() => {
            void approve(request);
          }}
        >
          {phase === "approving" ? (
            <LoaderCircle
              data-icon="inline-start"
              className="motion-safe:animate-spin"
              aria-hidden="true"
            />
          ) : null}
          {phase === "approving" ? "Handing it over…" : "Approve"}
        </Button>
        <Button
          type="button"
          size="lg"
          variant="outline"
          disabled={busy}
          onClick={() => {
            void deny(request);
          }}
        >
          Deny
        </Button>
      </div>

      <p className="mt-5 border-t border-line-soft pt-4 text-xs leading-5 text-muted-foreground">
        Denying tells the CLI to stop waiting and hands over nothing. Closing the
        tab also hands over nothing, but the CLI then waits out its five-minute
        timeout.
      </p>
    </CliPanel>
  );
}

function Detail({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid gap-1 sm:grid-cols-[150px_minmax(0,1fr)] sm:gap-3">
      <dt className="text-ui text-muted-foreground">{label}</dt>
      <dd className="text-ui leading-[1.55]">{children}</dd>
    </div>
  );
}

function InvalidLink({ problem }: { problem: string }) {
  return (
    <CliPanel
      tone="warning"
      icon={<TriangleAlert className="size-[18px]" aria-hidden="true" />}
      title="This isn't a CLI login link"
      lead="Nothing has been handed over, and nothing will be from this page."
    >
      <p className="text-ui mt-5 border-t border-line-soft pt-5 leading-[1.55] text-subtle">
        {problem}
      </p>
      <p className="text-ui mt-3 leading-[1.55] text-subtle">
        <CliCode>simple auth login</CliCode> opens this page with a callback on{" "}
        <CliCode>127.0.0.1</CliCode>, a code challenge and a state. Run it again
        and use the link it opens rather than a saved or edited one.
      </p>
      <div className="mt-6 text-[13px]">
        <Link href="/domains" className="link">
          Back to domains →
        </Link>
      </div>
    </CliPanel>
  );
}
