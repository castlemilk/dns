import Link from "next/link";
import { CircleCheck } from "lucide-react";

import { CliCode, CliPanel } from "@/components/cli/cli-panel";

/**
 * Where the browser lands after the token was posted to the CLI's loopback
 * callback and the CLI answered 200 — which it only does after it has checked
 * the state, checked the code challenge and accepted the credential.
 *
 * So this page can honestly say the CLI took it. What it cannot say is that the
 * CLI finished writing it to disk; the terminal is what reports that, and this
 * page points at it rather than guessing.
 */
export function CliApproved() {
  return (
    <CliPanel
      tone="success"
      icon={<CircleCheck className="size-[18px]" aria-hidden="true" />}
      title="The CLI has the token"
      lead="It checked the state and the code challenge, accepted the token, and is saving it now. You can close this tab."
    >
      <div className="text-ui mt-6 flex flex-col gap-3 border-t border-line-soft pt-5 leading-[1.55] text-subtle">
        <p>
          Your terminal prints the <CliCode>deephost</CliCode> banner when the
          credential is stored. If it does not, the login did not finish and
          nothing was written.
        </p>
        <p>
          <CliCode>deephost status</CliCode> checks the connection.{" "}
          <CliCode>deephost auth logout</CliCode> deletes the stored credential
          from this machine.
        </p>
        <p>
          This browser tab still holds the same token. Locking the console does
          not revoke the CLI&rsquo;s copy — only rotating{" "}
          <CliCode>DNS_API_BEARER_TOKEN</CliCode> on the control plane does.
        </p>
      </div>

      <div className="mt-6 text-[13px]">
        <Link href="/domains" className="link">
          Back to domains →
        </Link>
      </div>
    </CliPanel>
  );
}
