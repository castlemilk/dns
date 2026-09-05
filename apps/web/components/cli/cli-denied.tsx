import Link from "next/link";
import { Ban } from "lucide-react";

import { CliCode, CliPanel } from "@/components/cli/cli-panel";

/**
 * The refusal page. Reached from Deny, and safe to reach any other way: it makes
 * no request and asserts nothing about the CLI beyond what declining means.
 */
export function CliDenied() {
  return (
    <CliPanel
      tone="muted"
      icon={<Ban className="size-[18px]" aria-hidden="true" />}
      title="Nothing was handed over"
      lead="The login was declined. The operator token stayed in this browser tab. You can close this tab."
    >
      <div className="text-ui mt-6 flex flex-col gap-3 border-t border-line-soft pt-5 leading-[1.55] text-subtle">
        <p>
          The CLI reports that the login was declined and exits without writing
          anything to <CliCode>~/.config/simple/config.json</CliCode>.
        </p>
        <p>
          If declining was a mistake, run <CliCode>simple auth login</CliCode>{" "}
          again — it draws a fresh state and code challenge, so the old link is
          of no use to anyone.
        </p>
        <p>
          If you did not start this login, nothing further is needed here. To be
          certain no other copy of the token is in use, rotate{" "}
          <CliCode>DNS_API_BEARER_TOKEN</CliCode> on the control plane.
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
