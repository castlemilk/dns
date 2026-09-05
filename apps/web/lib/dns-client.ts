"use client";

import { createClient, type Client } from "@connectrpc/connect";

import { DNSService } from "@/gen/dns/v1/dns_pb";
import { createOperatorTransport } from "@/lib/operator-transport";

export type DNSClient = Client<typeof DNSService>;

let client: DNSClient | undefined;

export function getDNSClient(): DNSClient {
  if (client) {
    return client;
  }

  // The bearer interceptor, the `Unauthenticated → lock` rule and the base URL live in
  // `lib/operator-transport.ts`; the phase-1 10 s per-call timeout is its default.
  client = createClient(DNSService, createOperatorTransport());
  return client;
}
