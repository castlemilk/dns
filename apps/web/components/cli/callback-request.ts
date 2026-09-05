/**
 * The link `simple auth login` opens, and the two answers the console posts back
 * to it.
 *
 * This is not OIDC and there is no identity provider. The control plane has one
 * static operator credential, and this flow hands that credential from a browser
 * tab that is already unlocked to a CLI running on the same machine. `state` and
 * `challenge` only bind the answer to one `simple auth login` invocation; they
 * authenticate nobody.
 *
 * The check in `parseCallbackRequest` is the control that keeps the credential on
 * this machine. Everything after it posts the operator token, so a `callback`
 * that is not `http://127.0.0.1:<port>/callback` is refused here, before any
 * request is made.
 */

/** base64url(sha256(verifier)) with no padding is exactly 43 characters. */
const challengePattern = /^[A-Za-z0-9_-]{43}$/;

/** The CLI's state is base64url of 16 random bytes; the bound is generous. */
const statePattern = /^[A-Za-z0-9_-]{16,128}$/;

/** The only path the CLI's loopback server answers on. */
const callbackPath = "/callback";

/**
 * `127.0.0.1` literally, not `localhost`: the CLI binds the literal address, and
 * a name would make this depend on how the machine happens to resolve it.
 */
const loopbackHost = "127.0.0.1";

/** A parameter is never echoed back in full; a long one would be the message. */
const maxEchoLength = 120;

/** Long enough for a loopback round trip, short enough to fail visibly. */
const requestTimeoutMs = 10_000;

/** The raw query string, exactly as the CLI wrote it. */
export type CallbackQuery = {
  callback?: string;
  challenge?: string;
  state?: string;
};

/** A checked request. `callbackUrl` is rebuilt from the parsed parts, so the
 * value posted to is the one that passed the checks and not the raw string. */
export type CallbackRequest = {
  callbackUrl: string;
  port: string;
  challenge: string;
  state: string;
};

export type ParsedCallbackRequest =
  | { ok: true; request: CallbackRequest }
  | { ok: false; problem: string };

function echo(value: string): string {
  return value.length > maxEchoLength
    ? `${value.slice(0, maxEchoLength)}…`
    : value;
}

/**
 * Checks the three query parameters. Failures use the control plane's
 * `field: message` convention so the sentence names the parameter at fault.
 */
export function parseCallbackRequest(
  query: CallbackQuery,
): ParsedCallbackRequest {
  const rawCallback = query.callback?.trim();
  if (!rawCallback) {
    return { ok: false, problem: "callback: the link is missing it." };
  }

  let url: URL;
  try {
    url = new URL(rawCallback);
  } catch {
    return { ok: false, problem: "callback: not a URL." };
  }

  if (url.protocol !== "http:") {
    return {
      ok: false,
      problem: `callback: must be http, not ${echo(url.protocol.replace(":", ""))}.`,
    };
  }
  if (url.username || url.password) {
    return { ok: false, problem: "callback: must not carry credentials." };
  }
  if (url.hostname !== loopbackHost) {
    return {
      ok: false,
      problem: `callback: must point at ${loopbackHost} on this machine, not ${echo(url.hostname)}.`,
    };
  }
  if (!url.port) {
    return { ok: false, problem: "callback: must name the port the CLI is listening on." };
  }
  if (url.pathname !== callbackPath) {
    return {
      ok: false,
      problem: `callback: must end in ${callbackPath}, not ${echo(url.pathname)}.`,
    };
  }
  if (url.search || url.hash) {
    return { ok: false, problem: "callback: must have no query string or fragment." };
  }

  const challenge = query.challenge?.trim() ?? "";
  if (!challengePattern.test(challenge)) {
    return {
      ok: false,
      problem: challenge
        ? "challenge: not an S256 code challenge."
        : "challenge: the link is missing it.",
    };
  }

  const state = query.state?.trim() ?? "";
  if (!statePattern.test(state)) {
    return {
      ok: false,
      problem: state
        ? "state: not a value this console can return."
        : "state: the link is missing it.",
    };
  }

  return {
    ok: true,
    request: {
      callbackUrl: `${url.origin}${callbackPath}`,
      port: url.port,
      challenge,
      state,
    },
  };
}

/**
 * Posts one JSON body to the checked loopback callback.
 *
 * `redirect: "error"` matters: without it a callback that answered with a
 * redirect would have the body — which holds the operator token on the approve
 * path — replayed to wherever it pointed. `credentials: "omit"` keeps cookies
 * out of a cross-origin request that needs none.
 */
async function postToCallback(
  request: CallbackRequest,
  body: Record<string, string>,
): Promise<void> {
  let response: Response;
  try {
    response = await fetch(request.callbackUrl, {
      method: "POST",
      mode: "cors",
      cache: "no-store",
      credentials: "omit",
      redirect: "error",
      referrerPolicy: "no-referrer",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(requestTimeoutMs),
    });
  } catch {
    // The caught reason is deliberately dropped rather than quoted: a fetch
    // failure can name the request, and the request body holds the credential.
    throw new Error(
      `${request.callbackUrl} didn't answer. The CLI may have stopped or timed out — run \`simple auth login\` again.`,
    );
  }

  if (!response.ok) {
    // The CLI's own body is not read: it is an OAuth-shaped error object whose
    // description adds nothing an operator can act on here.
    throw new Error(
      `The CLI refused this answer (HTTP ${response.status}) and stored nothing. Run \`simple auth login\` again and use the link it opens.`,
    );
  }
}

/**
 * Hands the operator token to the CLI.
 *
 * `challenge` is echoed rather than a PKCE verifier because the console is only
 * ever sent the challenge; the CLI accepts either half and keeps its verifier to
 * itself. The token travels in this body and nowhere else — not in the address
 * bar, not in a log, not in the CLI's terminal.
 */
export async function postApproval(
  request: CallbackRequest,
  token: string,
): Promise<void> {
  await postToCallback(request, {
    state: request.state,
    challenge: request.challenge,
    token,
  });
}

/**
 * Tells the CLI the login was declined, so it fails now instead of waiting out
 * its five-minute timeout. Best effort: nothing was handed over either way, so a
 * failure here does not change what the human is told next.
 */
export async function postDenial(request: CallbackRequest): Promise<void> {
  try {
    await postToCallback(request, {
      state: request.state,
      error: "access_denied",
    });
  } catch {
    // The CLI times out on its own; the browser still lands on the refusal page.
  }
}
