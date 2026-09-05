"use client";

import {
  Code,
  ConnectError,
  type Interceptor,
  type Transport,
} from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

import {
  getOperatorToken,
  lockOperatorSession,
} from "@/lib/operator-session";

/**
 * The one place that knows where the control API lives and how the operator token is
 * attached. Every browser call — Connect clients and the plain-fetch upload route —
 * goes through here, so an `Unauthenticated` answer locks the session exactly once and
 * no other module has to repeat the base-URL rule.
 */

/** `NEXT_PUBLIC_DNS_API_URL` or the current origin, without a trailing slash. */
export function getApiBaseUrl(): string {
  const configured = process.env.NEXT_PUBLIC_DNS_API_URL?.trim();
  const base =
    configured || (typeof window === "undefined" ? "" : window.location.origin);
  return base.replace(/\/$/, "");
}

const operatorAuthInterceptor: Interceptor = (next) => async (request) => {
  const token = getOperatorToken();
  if (token) {
    request.header.set("Authorization", `Bearer ${token}`);
  }

  try {
    return await next(request);
  } catch (caught) {
    if (ConnectError.from(caught).code === Code.Unauthenticated) {
      lockOperatorSession("unauthenticated");
    }
    throw caught;
  }
};

/**
 * A Connect transport carrying the operator bearer. `timeoutMs` is per call: the DNS
 * client keeps the phase-1 10 s, the engine facades ask for more because attach, bind
 * and checkout make one bounded call to an external engine inside the request.
 */
export function createOperatorTransport(options?: {
  timeoutMs?: number;
}): Transport {
  return createConnectTransport({
    baseUrl: getApiBaseUrl(),
    useBinaryFormat: false,
    defaultTimeoutMs: options?.timeoutMs ?? 10_000,
    interceptors: [operatorAuthInterceptor],
  });
}

const maxErrorTextLength = 300;

/**
 * `fetch` against the control API with the operator bearer attached. Used by the
 * routes that are not Connect procedures (the multipart folder upload). No default
 * timeout — the caller passes its own `signal`, because an upload is as slow as the
 * folder is large.
 */
export async function operatorFetch(
  path: string,
  init?: RequestInit,
): Promise<Response> {
  const headers = new Headers(init?.headers);
  const token = getOperatorToken();
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }

  const response = await fetch(`${getApiBaseUrl()}${path}`, {
    ...init,
    headers,
  });

  if (response.status === 401) {
    lockOperatorSession("unauthenticated");
  }

  if (!response.ok) {
    // The body is the server's own message; it is bounded here because an HTML error
    // page from a proxy would otherwise become the dialog's error text.
    let text = "";
    try {
      text = (await response.text()).trim();
    } catch {
      text = "";
    }
    throw new Error(
      text.slice(0, maxErrorTextLength) || `Request failed (${response.status}).`,
    );
  }

  return response;
}
