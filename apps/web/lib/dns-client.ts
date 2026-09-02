"use client";

import {
  Code,
  ConnectError,
  createClient,
  type Client,
  type Interceptor,
} from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

import { DNSService } from "@/gen/dns/v1/dns_pb";
import {
  getOperatorToken,
  lockOperatorSession,
} from "@/lib/operator-session";

export type DNSClient = Client<typeof DNSService>;

let client: DNSClient | undefined;

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

export function getDNSClient(): DNSClient {
  if (client) {
    return client;
  }

  const configuredBaseURL = process.env.NEXT_PUBLIC_DNS_API_URL?.trim();
  const baseUrl = (configuredBaseURL || window.location.origin).replace(/\/$/, "");

  client = createClient(
    DNSService,
    createConnectTransport({
      baseUrl,
      useBinaryFormat: false,
      defaultTimeoutMs: 10_000,
      interceptors: [operatorAuthInterceptor],
    }),
  );
  return client;
}
