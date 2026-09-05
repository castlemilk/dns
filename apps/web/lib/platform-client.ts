"use client";

import { createClient, type Client } from "@connectrpc/connect";

import { ActivityService } from "@/gen/activity/v1/activity_pb";
import { BillingService } from "@/gen/billing/v1/billing_pb";
import { HostingService } from "@/gen/hosting/v1/hosting_pb";
import { MailService } from "@/gen/mail/v1/mail_pb";
import { PlatformService } from "@/gen/platform/v1/platform_pb";
import { createOperatorTransport } from "@/lib/operator-transport";

/**
 * Clients for the engine facades. The browser never talks to DeepHost, Stalwart or
 * Stripe: every call goes to this repo's control API on the same origin, which holds
 * the engine credentials.
 *
 * 30 s per call, not the DNS client's 10 s: attach, bind, deploy and checkout each make
 * one bounded call to an engine inside the request.
 */
const engineTimeoutMs = 30_000;

let transport: ReturnType<typeof createOperatorTransport> | undefined;

function engineTransport() {
  if (!transport) {
    transport = createOperatorTransport({ timeoutMs: engineTimeoutMs });
  }
  return transport;
}

export type PlatformClient = Client<typeof PlatformService>;
export type HostingClient = Client<typeof HostingService>;
export type MailClient = Client<typeof MailService>;
export type BillingClient = Client<typeof BillingService>;
export type ActivityClient = Client<typeof ActivityService>;

let platformClient: PlatformClient | undefined;
let hostingClient: HostingClient | undefined;
let mailClient: MailClient | undefined;
let billingClient: BillingClient | undefined;
let activityClient: ActivityClient | undefined;

export function getPlatformClient(): PlatformClient {
  platformClient ??= createClient(PlatformService, engineTransport());
  return platformClient;
}

export function getHostingClient(): HostingClient {
  hostingClient ??= createClient(HostingService, engineTransport());
  return hostingClient;
}

export function getMailClient(): MailClient {
  mailClient ??= createClient(MailService, engineTransport());
  return mailClient;
}

export function getBillingClient(): BillingClient {
  billingClient ??= createClient(BillingService, engineTransport());
  return billingClient;
}

export function getActivityClient(): ActivityClient {
  activityClient ??= createClient(ActivityService, engineTransport());
  return activityClient;
}
