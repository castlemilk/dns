import type { Metadata } from "next";

import { CliAuthorize } from "@/components/cli/cli-authorize";

export const metadata: Metadata = { title: "Authorize the CLI" };

/**
 * `deephost auth login` opens
 * `/cli/authorize?callback=http://127.0.0.1:<port>/callback&challenge=<S256>&state=<state>`.
 * Those three names are the whole contract; the PKCE verifier is never sent
 * here, so the console proves it is answering that invocation by echoing the
 * challenge.
 *
 * The route sits in the `(app)` group so the operator gate has already run: the
 * screen cannot offer a token to anything until the human has unlocked this tab.
 * Every parameter is checked in the client component before a request is made.
 *
 * The props are written out rather than using the generated `PageProps<…>`
 * alias, which only exists after `next typegen` has seen this route.
 */
export default async function Page(props: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const params = await props.searchParams;
  const first = (name: string): string | undefined => {
    const value = params[name];
    return typeof value === "string" ? value : undefined;
  };

  return (
    <CliAuthorize
      callback={first("callback")}
      challenge={first("challenge")}
      state={first("state")}
    />
  );
}
