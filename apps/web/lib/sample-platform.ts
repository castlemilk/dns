import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";

import {
  SubscriptionState,
  DomainBillingSchema,
  type DomainBilling,
} from "@/gen/billing/v1/billing_pb";
import {
  DeployKind,
  DeployPhase,
  DeploySchema,
  Framework,
  HostnameRole,
  HostnameState,
  SiteState,
  SiteSchema,
  TimeSource,
  type Deploy,
  type Site,
} from "@/gen/hosting/v1/hosting_pb";
import {
  ForwarderKind,
  ForwarderSchema,
  MailDomainSchema,
  MailDomainState,
  MailboxSchema,
  type Forwarder,
  type MailDomain,
  type Mailbox,
} from "@/gen/mail/v1/mail_pb";

/**
 * Hard-coded engine data for the public landing page's product frame, the sibling of
 * `lib/sample-zone.ts` and — with it — the ONLY place in the app where data no engine
 * returned may be rendered (the honesty rule in the brief). `eslint.config.mjs`
 * restricts imports of this module to `components/marketing/product-frame.tsx`.
 *
 * Every value is in the form the facade would actually return: the site's records match
 * the zone in `lib/sample-zone.ts`, byte counts are raw `uint64` values, TXT values are
 * stored quoted and CNAME/MX targets carry the trailing dot.
 *
 * 64-bit fields are built with `BigInt(...)`: `600n` literals are a compile error under
 * `target: ES2017` (TS2737).
 *
 * The frame renders these without callbacks, so nothing here reaches a `RelativeTime`:
 * every date that appears on screen arrives as a frozen label (`sampleDeployedLabel`,
 * `sampleBillingLine`) and the prerendered HTML cannot hydrate to different text.
 */

const sampleInstant = new Date("2026-09-02T09:00:00Z");
const sampleTimestamp = timestampFromDate(sampleInstant);

const deployRequested = timestampFromDate(new Date("2026-09-02T08:47:22Z"));
const deployStarted = timestampFromDate(new Date("2026-09-02T08:47:26Z"));
const deployFinished = timestampFromDate(new Date("2026-09-02T08:48:04Z"));
const deployLive = timestampFromDate(new Date("2026-09-02T08:48:11Z"));

/**
 * The frame's replacement for every deploy-time `RelativeTime` in the Website card,
 * frozen as a literal for the same reason as `sampleUpdatedLabel`.
 */
export const sampleDeployedLabel = "12 min ago";

/** The frame's `DomainHeader` billing line — a fixed string, never a formatted date. */
export const sampleBillingLine = "Subscription: active · renews 14 Mar 27";

const gigabyte = 1024 * 1024 * 1024;

// `timeSource: ENGINE` on purpose: the frame must never render the "≈38s (observed)"
// path, which carries a caption about this control plane's own clock.
const sampleDeploy: Deploy = create(DeploySchema, {
  id: "sample-deploy-live",
  zoneId: "sample-zone",
  zoneName: "acme.dev",
  kind: DeployKind.GIT,
  phase: DeployPhase.LIVE,
  reason: "",
  buildId: "b-1a2b3c4d5e6f",
  releaseId: "rel-b-1a2b3c4d5e6f",
  source: {
    repository: "https://github.com/acme/site",
    revision: "main",
    path: "",
    privateRepository: false,
    uploadName: "",
    uploadFiles: 0,
    uploadBytes: BigInt(0),
  },
  framework: Framework.NEXTJS,
  requestedAt: deployRequested,
  startedAt: deployStarted,
  finishedAt: deployFinished,
  liveAt: deployLive,
  timeSource: TimeSource.ENGINE,
  buildSeconds: 38,
  live: true,
  logSnapshot: true,
  rolledBackFrom: "",
});

export const sampleSite: Site = create(SiteSchema, {
  zoneId: "sample-zone",
  zoneName: "acme.dev",
  app: "acme-dev-1a2b3c",
  appRef: "deep-acme-dev-1a2b3c",
  framework: Framework.NEXTJS,
  repository: "https://github.com/acme/site",
  branch: "main",
  path: "",
  state: SiteState.READY,
  reason: "",
  www: true,
  hostnames: [
    {
      host: "acme.dev",
      role: HostnameRole.APEX,
      state: HostnameState.CERTIFICATE_READY,
      tlsMode: "issued",
      reason: "",
      observedAt: sampleTimestamp,
      retrying: false,
    },
    {
      host: "www.acme.dev",
      role: HostnameRole.WWW,
      state: HostnameState.CERTIFICATE_READY,
      tlsMode: "issued",
      reason: "",
      observedAt: sampleTimestamp,
      retrying: false,
    },
    {
      host: "deep-acme-dev-1a2b3c.apps.deephost.dev",
      role: HostnameRole.AUTO,
      state: HostnameState.ROUTES_READY,
      tlsMode: "shared",
      reason: "",
      observedAt: sampleTimestamp,
      retrying: false,
    },
  ],
  autoHostname: "deep-acme-dev-1a2b3c.apps.deephost.dev",
  dns: {
    inSync: true,
    apexAddresses: ["76.76.21.21"],
    wwwTarget: "acme.dev.",
    recordIds: ["sample-a-apex", "sample-cname-www"],
    reconciledAt: sampleTimestamp,
    problems: [],
  },
  liveDeployId: "sample-deploy-live",
  latestDeployId: "sample-deploy-live",
  live: sampleDeploy,
  latest: sampleDeploy,
  createdAt: sampleTimestamp,
  updatedAt: sampleTimestamp,
  zoneMissing: false,
});

export const sampleMailDomain: MailDomain = create(MailDomainSchema, {
  zoneId: "sample-zone",
  zoneName: "acme.dev",
  engineDomainId: "sample-mail-domain",
  state: MailDomainState.BOUND,
  reason: "",
  mailHostname: "mx1.deephost.dev",
  dmarcPolicy: "quarantine",
  reportAddress: "dmarc@acme.dev",
  dkim: [
    {
      selector: "s1",
      algorithm: "rsa",
      stage: "active",
      published: true,
      createdAt: sampleTimestamp,
    },
  ],
  records: [
    {
      name: "@",
      type: "MX",
      value: "10 mx1.deephost.dev.",
      ttl: 3600,
      recordId: "sample-mx-1",
      present: true,
      matches: true,
    },
    {
      name: "@",
      type: "TXT",
      value: '"v=spf1 include:spf.deephost.dev -all"',
      ttl: 3600,
      recordId: "sample-txt-spf",
      present: true,
      matches: true,
    },
    {
      name: "s1._domainkey",
      type: "TXT",
      value:
        '"v=DKIM1; k=rsa; p=MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQ…"',
      ttl: 300,
      recordId: "sample-txt-dkim",
      present: true,
      matches: true,
    },
    {
      name: "_dmarc",
      type: "TXT",
      value: '"v=DMARC1; p=quarantine; rua=mailto:dmarc@acme.dev"',
      ttl: 300,
      recordId: "sample-txt-dmarc",
      present: true,
      matches: true,
    },
    // The client-autoconfiguration records a real binding publishes by default
    // (§G4): the RFC 6186 / RFC 6764 SRV records and the two client aliases. The
    // card counts these to decide what its "Not published" line may still claim,
    // so leaving them out of the sample understated the product on the landing
    // page. `delivery` is deliberately absent: delivery counts need a webhook
    // receiver this deployment may not have, and the frame has no way to know.
    ...(
      [
        ["_submissions._tcp", "0 1 465 mx1.deephost.dev."],
        ["_imaps._tcp", "0 1 993 mx1.deephost.dev."],
        ["_pop3s._tcp", "0 1 995 mx1.deephost.dev."],
        ["_jmap._tcp", "0 1 443 mx1.deephost.dev."],
        ["_caldavs._tcp", "0 1 443 mx1.deephost.dev."],
        ["_carddavs._tcp", "0 1 443 mx1.deephost.dev."],
      ] as const
    ).map(([name, value]) => ({
      name,
      type: "SRV",
      value,
      ttl: 3600,
      recordId: `sample-srv-${name}`,
      present: true,
      matches: true,
    })),
    ...(["autoconfig", "autodiscover"] as const).map((name) => ({
      name,
      type: "CNAME",
      value: "mx1.deephost.dev.",
      ttl: 3600,
      recordId: `sample-cname-${name}`,
      present: true,
      matches: true,
    })),
  ],
  recordsInSync: true,
  mailboxCount: 3,
  mailboxLimit: 5,
  usedBytes: BigInt(3006477107),
  forwarderCount: 1,
  hasPostmaster: true,
  countsAvailable: true,
  createdAt: sampleTimestamp,
  reconciledAt: sampleTimestamp,
  dkimPending: false,
  zoneMissing: false,
  publishClientAutoconfig: true,
});

export const sampleMailboxes: Mailbox[] = [
  create(MailboxSchema, {
    id: "sample-mailbox-mara",
    zoneId: "sample-zone",
    address: "mara@acme.dev",
    localPart: "mara",
    displayName: "Mara Ede",
    quotaBytes: BigInt(5 * gigabyte),
    usedBytes: BigInt(2254857830),
    aliases: [],
    createdAt: sampleTimestamp,
  }),
  create(MailboxSchema, {
    id: "sample-mailbox-team",
    zoneId: "sample-zone",
    address: "team@acme.dev",
    localPart: "team",
    displayName: "Team",
    quotaBytes: BigInt(5 * gigabyte),
    usedBytes: BigInt(536870912),
    aliases: [],
    createdAt: sampleTimestamp,
  }),
  create(MailboxSchema, {
    id: "sample-mailbox-postmaster",
    zoneId: "sample-zone",
    address: "postmaster@acme.dev",
    localPart: "postmaster",
    displayName: "Postmaster",
    quotaBytes: BigInt(gigabyte),
    usedBytes: BigInt(214748365),
    aliases: [],
    createdAt: sampleTimestamp,
  }),
];

export const sampleForwarders: Forwarder[] = [
  create(ForwarderSchema, {
    id: "sample-forwarder-hello",
    zoneId: "sample-zone",
    address: "hello@acme.dev",
    localPart: "hello",
    kind: ForwarderKind.ALIAS,
    targets: ["mara@acme.dev"],
    external: false,
  }),
];

export const sampleBilling: DomainBilling = create(DomainBillingSchema, {
  zoneId: "sample-zone",
  zoneName: "acme.dev",
  state: SubscriptionState.ACTIVE,
  subscriptionId: "sub_sample",
  currentPeriodEnd: timestampFromDate(new Date("2027-03-14T09:00:00Z")),
  updatedAt: sampleTimestamp,
  lastEventId: "",
  lastInvoiceStatus: "paid",
  checkoutSessionId: "",
  zoneMissing: false,
});
