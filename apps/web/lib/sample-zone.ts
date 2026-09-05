import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";

import { RecordSource, RecordType, ZoneSchema, type Zone } from "@/gen/dns/v1/dns_pb";

/**
 * Hard-coded sample zone for the public landing page's product frame — the ONLY place
 * in the app where data the control API did not return may be rendered (see the honesty
 * rule in the brief). `eslint.config.mjs` restricts imports of this module to
 * `components/marketing/product-frame.tsx`.
 *
 * Every value is in the form the control API would actually return: names relative to
 * the zone (`@` for the apex), CNAME/MX/NS targets fully qualified with a trailing dot,
 * TXT values quoted, and the managed SOA/NS rows the control plane owns.
 */

const sampleInstant = new Date("2026-09-02T09:00:00Z");
const sampleTimestamp = timestampFromDate(sampleInstant);

/**
 * `formatShortDate(sampleInstant)`, frozen as a literal: the landing page is
 * prerendered once and must not hydrate to a different string or drift with the
 * viewer's clock or time zone.
 */
export const sampleUpdatedLabel = "02 Sep 26";

const sampleSerial = 2026090201;

type SampleRecord = {
  id: string;
  name: string;
  type: RecordType;
  ttl: number;
  value: string;
  managed?: boolean;
  source?: RecordSource;
};

// User records first in the order the design's raw zone lists them, managed rows last —
// the same order the raw zone table and the DNS card groups use.
const sampleRecords: SampleRecord[] = [
  {
    id: "sample-a-apex",
    name: "@",
    type: RecordType.A,
    ttl: 300,
    value: "76.76.21.21",
    source: RecordSource.HOSTING,
  },
  {
    id: "sample-cname-www",
    name: "www",
    type: RecordType.CNAME,
    ttl: 300,
    value: "acme.dev.",
    source: RecordSource.HOSTING,
  },
  {
    id: "sample-mx-1",
    name: "@",
    type: RecordType.MX,
    ttl: 300,
    value: "10 mx1.simple.host.",
    source: RecordSource.MAIL,
  },
  {
    id: "sample-txt-spf",
    name: "@",
    type: RecordType.TXT,
    ttl: 3600,
    value: '"v=spf1 include:spf.simple.host -all"',
    source: RecordSource.MAIL,
  },
  {
    id: "sample-txt-dkim",
    name: "s1._domainkey",
    type: RecordType.TXT,
    ttl: 300,
    value:
      '"v=DKIM1; k=rsa; p=MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQ…"',
    source: RecordSource.MAIL,
  },
  {
    id: "sample-txt-dmarc",
    name: "_dmarc",
    type: RecordType.TXT,
    ttl: 300,
    value: '"v=DMARC1; p=quarantine; rua=mailto:dmarc@acme.dev"',
    source: RecordSource.MAIL,
  },
  {
    id: "sample-txt-google",
    name: "@",
    type: RecordType.TXT,
    ttl: 3600,
    value: '"google-site-verification=9xk2…"',
  },
  {
    id: "sample-cname-api",
    name: "api",
    type: RecordType.CNAME,
    ttl: 300,
    value: "acme-api.fly.dev.",
  },
  {
    id: "sample-soa",
    name: "@",
    type: RecordType.SOA,
    ttl: 3600,
    // The control plane's soaValue format: <ns1> hostmaster.<zone>. <serial> 3600 600 1209600 300
    value: `ns1.simple.host. hostmaster.acme.dev. ${sampleSerial} 3600 600 1209600 300`,
    managed: true,
  },
  {
    id: "sample-ns-1",
    name: "@",
    type: RecordType.NS,
    ttl: 3600,
    value: "ns1.simple.host.",
    managed: true,
  },
  {
    id: "sample-ns-2",
    name: "@",
    type: RecordType.NS,
    ttl: 3600,
    value: "ns2.simple.host.",
    managed: true,
  },
];

export const sampleZone: Zone = create(ZoneSchema, {
  id: "sample-zone",
  name: "acme.dev",
  serial: sampleSerial,
  createdAt: sampleTimestamp,
  updatedAt: sampleTimestamp,
  nameservers: ["ns1.simple.host.", "ns2.simple.host."],
  records: sampleRecords.map((record) => ({
    id: record.id,
    name: record.name,
    type: record.type,
    ttl: record.ttl,
    value: record.value,
    managed: record.managed ?? false,
    source: record.source ?? RecordSource.USER,
    createdAt: sampleTimestamp,
    updatedAt: sampleTimestamp,
  })),
});
