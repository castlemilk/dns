import {
  type Record as DNSRecord,
  RecordSource as ProtoRecordSource,
  RecordType,
  type ServerStatus,
  type Zone,
} from "@/gen/dns/v1/dns_pb";
import {
  type Dmarc,
  type Mx,
  type Spf,
  connectHref,
  fqdn,
  isDkimName,
  isHostname,
  isSpfText,
  lower,
  parseDmarc,
  parseMx,
  parseSpf,
  recordTypeName,
  sameHost,
  stripDot,
  unquoteTxt,
  zoneHref,
} from "@/lib/dns-values";
import { plural, toDate } from "@/lib/format";
import {
  guessMailProvider,
  recogniseHost,
  recogniseTxt,
} from "@/lib/service-presets";

/**
 * The only place that classifies records and derives the website / email / DNS /
 * attention / dashboard view-models. Everything here is a reading of the zone the
 * control API returned — no fabricated state, no claim about who wrote a record.
 */

export type RecordSource = "managed" | "website" | "email" | "custom";

/**
 * Who wrote the record, straight from `Record.source`. The shape-based `source` above
 * groups records by what they do; `origin` is the only field that says who owns them —
 * an engine record is rewritten by its reconciler, so the UI marks it and the control
 * plane refuses edits to it.
 */
export type RecordOrigin = "user" | "hosting" | "mail";

export function recordOrigin(r: DNSRecord): RecordOrigin {
  switch (r.source) {
    case ProtoRecordSource.HOSTING:
      return "hosting";
    case ProtoRecordSource.MAIL:
      return "mail";
    default:
      // USER and UNSPECIFIED (a record stored before provenance existed) are both the
      // operator's own record.
      return "user";
  }
}

export type ClassifiedRecord = {
  record: DNSRecord;
  source: RecordSource;
  origin: RecordOrigin;
  /** Unquoted TXT text, for TXT records only. */
  text?: string;
};

export function classifyRecord(r: DNSRecord): ClassifiedRecord {
  const origin = recordOrigin(r);
  if (r.managed) {
    return { record: r, source: "managed", origin };
  }
  // Provenance beats shape. The shape heuristics below only recognise the
  // records the engines published first — an apex A, an MX, SPF, DKIM, DMARC —
  // so the mail engine's client-autoconfiguration set (the _tcp SRV records and
  // the autoconfig/autodiscover aliases) fell through to "custom" and was
  // presented under "Anything you add yourself is listed here". That is a claim
  // about who wrote them, and it was the wrong one: the control plane refuses
  // to let the operator edit or delete them, and the reconciler rewrites them.
  // Record.source says who owns them, so it decides.
  if (origin === "hosting") {
    return { record: r, source: "website", origin };
  }
  if (origin === "mail") {
    return {
      record: r,
      source: "email",
      origin,
      ...(r.type === RecordType.TXT ? { text: unquoteTxt(r.value) } : {}),
    };
  }
  const name = lower(r.name);
  switch (r.type) {
    case RecordType.A:
    case RecordType.AAAA:
      return {
        record: r,
        source: name === "@" || name === "www" ? "website" : "custom",
        origin,
      };
    case RecordType.CNAME:
      if (name === "www") {
        return { record: r, source: "website", origin };
      }
      if (isDkimName(name)) {
        return { record: r, source: "email", origin };
      }
      return { record: r, source: "custom", origin };
    case RecordType.MX:
      return { record: r, source: name === "@" ? "email" : "custom", origin };
    case RecordType.TXT: {
      const text = unquoteTxt(r.value);
      if (name === "@" && isSpfText(text)) {
        return { record: r, source: "email", origin, text };
      }
      if (name === "_dmarc") {
        return { record: r, source: "email", origin, text };
      }
      if (isDkimName(name)) {
        return { record: r, source: "email", origin, text };
      }
      return { record: r, source: "custom", origin, text };
    }
    case RecordType.CAA:
      return { record: r, source: name === "@" ? "website" : "custom", origin };
    default:
      return { record: r, source: "custom", origin };
  }
}

/* -------------------------------------------------------------------------- */
/* Website                                                                     */
/* -------------------------------------------------------------------------- */

export type WebsiteTarget = {
  recordId: string;
  type: "A" | "AAAA" | "CNAME";
  /** Display value: trailing dot stripped. */
  value: string;
  /** Stored value, verbatim. */
  raw: string;
  ttl: number;
};

export type WebsiteCaa = {
  recordId: string;
  flags: number;
  tag: string;
  value: string;
};

export type WebsiteStatus = {
  kind: "not_set_up" | "pointed" | "www_only";
  apex: WebsiteTarget[];
  www: {
    kind: "same" | "elsewhere" | "missing" | "dangling";
    targets: WebsiteTarget[];
    service?: string;
  };
  wildcard?: WebsiteTarget[];
  caa: WebsiteCaa[];
  types: Array<"A" | "AAAA" | "CNAME">;
  url?: string;
  updatedAt?: Date;
};

const pointingTypes: Array<[RecordType, "A" | "AAAA" | "CNAME"]> = [
  [RecordType.A, "A"],
  [RecordType.AAAA, "AAAA"],
  [RecordType.CNAME, "CNAME"],
];

function pointingTypeName(
  type: RecordType,
): "A" | "AAAA" | "CNAME" | undefined {
  for (const [recordType, name] of pointingTypes) {
    if (recordType === type) {
      return name;
    }
  }
  return undefined;
}

function toTarget(record: DNSRecord): WebsiteTarget | undefined {
  const type = pointingTypeName(record.type);
  if (!type) {
    return undefined;
  }
  return {
    recordId: record.id,
    type,
    value: stripDot(record.value),
    raw: record.value,
    ttl: record.ttl,
  };
}

function targets(records: DNSRecord[]): WebsiteTarget[] {
  const out: WebsiteTarget[] = [];
  for (const record of records) {
    const target = toTarget(record);
    if (target) {
      out.push(target);
    }
  }
  return out;
}

function latestUpdate(records: DNSRecord[]): Date | undefined {
  let latest: Date | undefined;
  for (const record of records) {
    const date = toDate(record.updatedAt);
    if (date && (!latest || date.getTime() > latest.getTime())) {
      latest = date;
    }
  }
  return latest;
}

function sameValueSet(a: WebsiteTarget[], b: WebsiteTarget[]): boolean {
  if (a.length !== b.length) {
    return false;
  }
  const left = new Set(a.map((target) => lower(target.value)));
  const right = new Set(b.map((target) => lower(target.value)));
  if (left.size !== right.size) {
    return false;
  }
  for (const value of left) {
    if (!right.has(value)) {
      return false;
    }
  }
  return true;
}

function parseCaa(record: DNSRecord): WebsiteCaa | undefined {
  const match = /^([0-9]+)\s+(\S+)\s+(.*)$/.exec(record.value.trim());
  if (!match) {
    return undefined;
  }
  return {
    recordId: record.id,
    flags: Number(match[1]),
    tag: match[2],
    value: unquoteTxt(match[3]),
  };
}

export function deriveWebsite(
  zone: Zone,
  rows: ClassifiedRecord[],
): WebsiteStatus {
  const site = rows.filter((row) => row.source === "website");
  const siteRecords = site.map((row) => row.record);
  const apex = targets(
    siteRecords.filter(
      (record) =>
        lower(record.name) === "@" &&
        (record.type === RecordType.A || record.type === RecordType.AAAA),
    ),
  );
  const www = targets(
    siteRecords.filter((record) => lower(record.name) === "www"),
  );
  const caa: WebsiteCaa[] = [];
  for (const record of siteRecords) {
    if (record.type !== RecordType.CAA || lower(record.name) !== "@") {
      continue;
    }
    const parsed = parseCaa(record);
    if (parsed) {
      caa.push(parsed);
    }
  }

  const wwwAliasOfZone =
    www.length === 1 && www[0].type === "CNAME" && sameHost(www[0].raw, zone.name);
  const wwwKind: WebsiteStatus["www"]["kind"] =
    www.length === 0
      ? "missing"
      : wwwAliasOfZone && apex.length === 0
        ? "dangling"
        : wwwAliasOfZone
          ? "same"
          : apex.length > 0 &&
              www.every((target) => target.type !== "CNAME") &&
              sameValueSet(www, apex)
            ? "same"
            : "elsewhere";
  const service =
    wwwKind === "elsewhere" && www[0]?.type === "CNAME"
      ? recogniseHost(www[0].value)
      : undefined;

  const wildcardTargets = targets(
    rows
      .filter((row) => row.source !== "managed" && lower(row.record.name) === "*")
      .map((row) => row.record),
  );

  const types: Array<"A" | "AAAA" | "CNAME"> = [];
  for (const [, name] of pointingTypes) {
    if ([...apex, ...www].some((target) => target.type === name)) {
      types.push(name);
    }
  }

  const kind: WebsiteStatus["kind"] = apex.length
    ? "pointed"
    : www.length && wwwKind !== "dangling"
      ? "www_only"
      : "not_set_up";

  return {
    kind,
    apex,
    www: { kind: wwwKind, targets: www, service },
    wildcard: wildcardTargets.length ? wildcardTargets : undefined,
    caa,
    types,
    // The backend only rejects whitespace, control characters and `; $ ( ) \ "` in a zone
    // name, so `evil.com/x.victim.com` and `user:pw@evil.com` are storable and would turn
    // this into a link to another host. Only a plain hostname becomes a URL; anything else
    // leaves `url` undefined and the header falls back to a disabled "Visit site".
    url: !isHostname(zone.name)
      ? undefined
      : kind === "pointed"
        ? `https://${zone.name}`
        : kind === "www_only"
          ? `https://www.${zone.name}`
          : undefined,
    updatedAt: latestUpdate(siteRecords),
  };
}

/* -------------------------------------------------------------------------- */
/* Email                                                                       */
/* -------------------------------------------------------------------------- */

export type EmailMx = Mx & { recordId: string; ttl: number };

export type EmailStatus = {
  kind: "not_set_up" | "partial" | "routed" | "no_mail";
  mx: EmailMx[];
  nullMx: boolean;
  spf: { records: DNSRecord[]; parsed?: Spf };
  dkim: Array<{ selector: string; via: "TXT" | "CNAME"; recordId: string }>;
  dmarc: { record?: DNSRecord; parsed?: Dmarc };
  provider?: string;
  protection: { spf: boolean; dkim: boolean; dmarc: boolean };
  updatedAt?: Date;
};

export function deriveEmail(zone: Zone, rows: ClassifiedRecord[]): EmailStatus {
  const mail = rows.filter((row) => row.source === "email");
  const mailRecords = mail.map((row) => row.record);

  const mx: EmailMx[] = [];
  for (const record of mailRecords) {
    if (record.type !== RecordType.MX) {
      continue;
    }
    const parsed = parseMx(record.value);
    if (parsed) {
      mx.push({ ...parsed, recordId: record.id, ttl: record.ttl });
    }
  }
  mx.sort((a, b) =>
    a.priority !== b.priority
      ? a.priority - b.priority
      : a.host.localeCompare(b.host),
  );
  const nullMx = mx.length === 1 && mx[0].priority === 0 && mx[0].host === "";

  const spfRecords = mailRecords.filter(
    (record) => record.type === RecordType.TXT && lower(record.name) === "@",
  );
  const spfParsed = spfRecords.length
    ? parseSpf(unquoteTxt(spfRecords[0].value))
    : undefined;

  const dkim: EmailStatus["dkim"] = [];
  for (const record of mailRecords) {
    const name = lower(record.name);
    if (!isDkimName(name)) {
      continue;
    }
    if (record.type === RecordType.TXT) {
      dkim.push({
        selector: name.replace(/\._domainkey$/, ""),
        via: "TXT",
        recordId: record.id,
      });
    } else if (record.type === RecordType.CNAME) {
      dkim.push({
        selector: name.replace(/\._domainkey$/, ""),
        via: "CNAME",
        recordId: record.id,
      });
    }
  }

  const dmarcRecord = mailRecords.find(
    (record) => record.type === RecordType.TXT && lower(record.name) === "_dmarc",
  );
  const dmarcParsed = dmarcRecord
    ? parseDmarc(unquoteTxt(dmarcRecord.value))
    : undefined;

  const spfOnlyDenies =
    spfParsed !== undefined &&
    spfParsed.mechanisms.length === 0 &&
    !spfParsed.redirect &&
    spfParsed.all === "-all";

  const kind: EmailStatus["kind"] =
    nullMx || (mx.length === 0 && spfOnlyDenies)
      ? "no_mail"
      : mx.length === 0
        ? spfRecords.length || dkim.length || dmarcRecord
          ? "partial"
          : "not_set_up"
        : spfRecords.length === 1 && dmarcParsed?.valid
          ? "routed"
          : "partial";

  return {
    kind,
    mx,
    nullMx,
    spf: { records: spfRecords, parsed: spfParsed },
    dkim,
    dmarc: { record: dmarcRecord, parsed: dmarcParsed },
    provider: guessMailProvider(mx.map((entry) => entry.host)),
    protection: {
      spf: spfRecords.length > 0,
      dkim: dkim.length > 0,
      dmarc: dmarcParsed?.valid === true,
    },
    updatedAt: latestUpdate(mailRecords),
  };
}

/* -------------------------------------------------------------------------- */
/* DNS summary and card groups                                                 */
/* -------------------------------------------------------------------------- */

export type DnsSummary = {
  rows: ClassifiedRecord[];
  total: number;
  managedCount: number;
  userCount: number;
  nameservers: string[];
};

export type DnsGroup = {
  key: string;
  label: string;
  detail: string;
  /**
   * How `detail` reads: "success" for a group every record of which an engine writes
   * (detail "automatic"), otherwise the muted type list. Undefined means muted.
   */
  detailTone?: "success" | "muted";
  title: string;
  tone: "success" | "info" | "muted";
  recordIds: string[];
};

/**
 * True only when every record in the group was written by the hosting or mail engine,
 * which is the one case where "automatic" is a true statement about the group.
 */
function allEngineOwned(members: ClassifiedRecord[]): boolean {
  return members.length > 0 && members.every((member) => member.origin !== "user");
}

const websiteTypeOrder = [
  RecordType.A,
  RecordType.AAAA,
  RecordType.CNAME,
  RecordType.CAA,
];
const emailTypeOrder = [RecordType.MX, RecordType.TXT, RecordType.CNAME];

function typeSummary(
  rows: ClassifiedRecord[],
  order: RecordType[],
): { list: string; counts: string } {
  const counts = new Map<RecordType, number>();
  for (const row of rows) {
    counts.set(row.record.type, (counts.get(row.record.type) ?? 0) + 1);
  }
  const ordered = [
    ...order.filter((type) => counts.has(type)),
    ...[...counts.keys()].filter((type) => !order.includes(type)),
  ];
  return {
    list: ordered.map((type) => recordTypeName(type)).join(", "),
    counts: ordered
      .map((type) => `${recordTypeName(type)} ×${counts.get(type) ?? 0}`)
      .join(", "),
  };
}

function truncate(text: string, max: number): string {
  return text.length > max ? `${text.slice(0, max)}…` : text;
}

function customLabel(
  zone: Zone,
  members: ClassifiedRecord[],
): { label: string; title: string } {
  const first = members[0].record;
  const name = lower(first.name);
  const displayName = name === "*" ? `*.${zone.name}` : fqdn(zone.name, name);
  const values = members.map((member) => member.record.value);

  switch (first.type) {
    case RecordType.TXT: {
      const text = members[0].text ?? unquoteTxt(first.value);
      const known = recogniseTxt(text, name);
      return {
        label: known ?? `${displayName} · TXT "${truncate(text, 28)}"`,
        title: text,
      };
    }
    case RecordType.CNAME: {
      const target = stripDot(first.value);
      const service = recogniseHost(first.value);
      return {
        label: `${displayName} → ${service ?? target}`,
        title: `${displayName} → ${target}`,
      };
    }
    case RecordType.A:
    case RecordType.AAAA: {
      const label = `${displayName} → ${values.join(", ")}`;
      return { label, title: label };
    }
    case RecordType.MX: {
      const hosts = values.map((value) => {
        const parsed = parseMx(value);
        if (!parsed) {
          return value;
        }
        return parsed.host === "" ? "." : parsed.host;
      });
      const label = `Mail for ${displayName} → ${hosts.join(", ")}`;
      return { label, title: label };
    }
    case RecordType.NS: {
      const label = `${displayName} delegated to ${values
        .map((value) => stripDot(value))
        .join(", ")}`;
      return { label, title: label };
    }
    case RecordType.SRV: {
      const label = `${displayName} service → ${values.join(", ")}`;
      return { label, title: label };
    }
    case RecordType.CAA: {
      const rules = members.map((member) => {
        const parsed = parseCaa(member.record);
        return parsed ? `${parsed.tag} ${parsed.value}` : member.record.value;
      });
      const label = `Certificate authority rule (${rules.join(", ")})`;
      return { label, title: label };
    }
    default: {
      const label = `${displayName} · ${recordTypeName(first.type)}`;
      return { label, title: `${label} → ${values.join(", ")}` };
    }
  }
}

export function deriveGroups(
  zone: Zone,
  rows: ClassifiedRecord[],
  website: WebsiteStatus,
  email: EmailStatus,
): DnsGroup[] {
  const ids = (members: ClassifiedRecord[]) =>
    members.map((member) => member.record.id);
  const groups: DnsGroup[] = [];

  const site = rows.filter((row) => row.source === "website");
  if (site.length) {
    const parts = [
      website.apex.length ? zone.name : undefined,
      website.www.targets.length ? "www" : undefined,
    ].filter((part): part is string => part !== undefined);
    const summary = typeSummary(site, websiteTypeOrder);
    const automatic = allEngineOwned(site);
    groups.push({
      key: "website",
      label: parts.length ? `Website (${parts.join(", ")})` : "Website",
      detail: automatic ? "automatic" : summary.list,
      detailTone: automatic ? "success" : "muted",
      title: automatic ? `written by the Website engine · ${summary.counts}` : summary.counts,
      tone: "success",
      recordIds: ids(site),
    });
  }

  const mail = rows.filter((row) => row.source === "email");
  if (mail.length) {
    const summary = typeSummary(mail, emailTypeOrder);
    const automatic = allEngineOwned(mail);
    groups.push({
      key: "email",
      label: email.mx.length ? "Email + spam protection" : "Spam protection",
      detail: automatic ? "automatic" : summary.list,
      detailTone: automatic ? "success" : "muted",
      title: automatic ? `written by the Email engine · ${summary.counts}` : summary.counts,
      tone: "success",
      recordIds: ids(mail),
    });
  }

  const customKeys: string[] = [];
  const customMembers = new Map<string, ClassifiedRecord[]>();
  for (const row of rows) {
    if (row.source !== "custom") {
      continue;
    }
    const key =
      row.record.type === RecordType.TXT
        ? `custom:txt:${row.record.id}`
        : `custom:${lower(row.record.name)}:${row.record.type}`;
    const existing = customMembers.get(key);
    if (existing) {
      existing.push(row);
    } else {
      customMembers.set(key, [row]);
      customKeys.push(key);
    }
  }
  for (const key of customKeys) {
    const members = customMembers.get(key) ?? [];
    if (!members.length) {
      continue;
    }
    const { label, title } = customLabel(zone, members);
    groups.push({
      key,
      label,
      detail: `custom · ${recordTypeName(members[0].record.type)}`,
      title,
      tone: "info",
      recordIds: ids(members),
    });
  }

  const managed = rows.filter((row) => row.source === "managed");
  if (managed.length) {
    groups.push({
      key: "managed",
      label: "Zone authority",
      detail: "managed · SOA, NS",
      title:
        "SOA and apex NS records are created and updated by the control plane",
      tone: "muted",
      recordIds: ids(managed),
    });
  }

  return groups;
}

/* -------------------------------------------------------------------------- */
/* Attention                                                                   */
/* -------------------------------------------------------------------------- */

export type Attention = {
  zoneName: string;
  severity: "warn" | "info";
  code: string;
  message: string;
  href: string;
  /** Link text for the item; the caller falls back to "Finish setup →". */
  cta?: string;
};

export function deriveAttention(
  zone: Zone,
  rows: ClassifiedRecord[],
  website: WebsiteStatus,
  email: EmailStatus,
): Attention[] {
  const items: Attention[] = [];
  const add = (severity: Attention["severity"], code: string, message: string) => {
    items.push({
      zoneName: zone.name,
      severity,
      code,
      message,
      href: code === "empty" ? connectHref(zone.name) : zoneHref(zone.name),
    });
  };

  const routes = email.mx.length > 0 && !email.nullMx;

  if (email.spf.records.length > 1) {
    add(
      "warn",
      "spf-multiple",
      `${email.spf.records.length} SPF records — receivers treat that as a failure`,
    );
  }
  if (routes && !email.spf.parsed) {
    add("warn", "spf-missing", "email has no SPF record");
  }
  if (routes && email.dmarc.record && !email.dmarc.parsed?.valid) {
    add("warn", "dmarc-invalid", "DMARC record isn't valid");
  }
  if (routes && !email.dmarc.record) {
    add("warn", "dmarc-missing", "email has no DMARC record");
  }
  if (website.kind === "www_only") {
    add(
      "warn",
      "apex-missing",
      `only www is pointed, ${zone.name} itself isn't`,
    );
  }

  if (website.www.kind === "dangling") {
    add(
      "info",
      "www-alias-dangling",
      `www.${zone.name} is an alias of ${zone.name}, which has no address yet`,
    );
  }
  if (email.kind === "partial" && email.mx.length === 0) {
    add("info", "mx-missing", "spam protection set but no mail servers");
  }
  if (website.kind === "pointed" && website.www.kind === "missing") {
    add("info", "www-missing", `www.${zone.name} isn't pointed`);
  }
  if (rows.every((row) => row.source === "managed")) {
    add("info", "empty", "nothing set up yet");
  }

  return items;
}

/* -------------------------------------------------------------------------- */
/* Per-domain model                                                            */
/* -------------------------------------------------------------------------- */

export type DomainModel = {
  zone: Zone;
  rows: ClassifiedRecord[];
  website: WebsiteStatus;
  email: EmailStatus;
  dns: DnsSummary;
  groups: DnsGroup[];
  attention: Attention[];
  tone: "success" | "warning" | "muted";
  healthLabel: string;
  serial: number;
  updatedAt?: Date;
  createdAt?: Date;
};

/**
 * The domain header's one-line verdict, from a list of attention items.
 *
 * Exported because the header sits above the Website, Email and DNS cards and is read
 * as a statement about all three: [deriveDomain] can only see the zone's records, so a
 * caller that also has the engines' rows must re-derive this line from the merged list.
 * "Nothing needs attention" above a degraded card would be the page contradicting itself.
 */
export function healthLine(attention: Attention[]): {
  tone: DomainModel["tone"];
  healthLabel: string;
} {
  const warnCount = attention.filter((item) => item.severity === "warn").length;
  const tone: DomainModel["tone"] = warnCount
    ? "warning"
    : attention.some((item) => item.code === "empty")
      ? "muted"
      : "success";
  return {
    tone,
    healthLabel:
      tone === "success"
        ? "Nothing needs attention"
        : tone === "warning"
          ? `${warnCount} ${warnCount === 1 ? "thing needs" : "things need"} attention`
          : "Nothing set up yet",
  };
}

export function deriveDomain(zone: Zone): DomainModel {
  const rows = zone.records.map(classifyRecord);
  const website = deriveWebsite(zone, rows);
  const email = deriveEmail(zone, rows);
  const groups = deriveGroups(zone, rows, website, email);
  const attention = deriveAttention(zone, rows, website, email);

  const managedCount = rows.filter((row) => row.source === "managed").length;
  const dns: DnsSummary = {
    rows,
    total: zone.records.length,
    managedCount,
    userCount: zone.records.length - managedCount,
    nameservers: zone.nameservers.map((ns) => stripDot(ns)),
  };

  const { tone, healthLabel } = healthLine(attention);

  return {
    zone,
    rows,
    website,
    email,
    dns,
    groups,
    attention,
    tone,
    healthLabel,
    serial: zone.serial,
    updatedAt: toDate(zone.updatedAt),
    createdAt: toDate(zone.createdAt),
  };
}

/* -------------------------------------------------------------------------- */
/* Dashboard                                                                   */
/* -------------------------------------------------------------------------- */

export type Cell = { text: string; tone: "default" | "muted" | "warning" };

export type DomainRow = {
  name: string;
  href: string;
  tone: DomainModel["tone"];
  website: Cell;
  email: Cell;
  dns: Cell;
  updatedAt?: Date;
};

const problemLabels: Array<[code: string, label: (count: number) => string]> = [
  ["spf-multiple", (count) => `${count} SPF records`],
  ["spf-missing", () => "no SPF"],
  ["dmarc-invalid", () => "invalid DMARC"],
  ["dmarc-missing", () => "no DMARC"],
];

function websiteCell(model: DomainModel): Cell {
  const { website } = model;
  if (website.kind === "pointed") {
    const extra = website.apex.length > 1 ? ` +${website.apex.length - 1}` : "";
    return {
      text: `Pointed · ${website.apex[0].value}${extra}`,
      tone: "default",
    };
  }
  if (website.kind === "www_only") {
    return { text: "www only", tone: "warning" };
  }
  return website.www.kind === "dangling"
    ? { text: "www alias only", tone: "muted" }
    : { text: "Not set up", tone: "muted" };
}

function emailCell(model: DomainModel): Cell {
  const { email } = model;
  if (email.kind === "routed") {
    return {
      text: `Routed · ${email.provider ?? stripDot(email.mx[0].host)}`,
      tone: "default",
    };
  }
  if (email.kind === "partial") {
    if (!email.mx.length) {
      return { text: "Spam protection only", tone: "warning" };
    }
    const codes = new Set(
      model.attention
        .filter((item) => item.severity === "warn")
        .map((item) => item.code),
    );
    const problems: string[] = [];
    for (const [code, label] of problemLabels) {
      if (codes.has(code)) {
        problems.push(label(email.spf.records.length));
      }
    }
    return {
      text: `Routed · ${problems.length ? problems.join(", ") : "check spam protection"}`,
      tone: "warning",
    };
  }
  if (email.kind === "no_mail") {
    return { text: "No mail", tone: "muted" };
  }
  return { text: "Not set up", tone: "muted" };
}

export function toDomainRow(model: DomainModel): DomainRow {
  const count = model.zone.records.length;
  return {
    name: model.zone.name,
    href: zoneHref(model.zone.name),
    tone: model.tone,
    website: websiteCell(model),
    email: emailCell(model),
    dns: {
      text: `Managed · ${count} ${plural(count, "record")}`,
      tone: "default",
    },
    updatedAt: model.updatedAt,
  };
}

export type Dashboard = {
  rows: DomainRow[];
  subtitle: string;
  attention: Attention[];
  latest?: {
    zoneName: string;
    serial: number;
    updatedAt: Date;
    recordCount: number;
  };
  server?: {
    queries: bigint;
    zones: number;
    records: number;
    startedAt?: Date;
  };
};

export function deriveDashboard(
  zones: Zone[],
  status?: ServerStatus,
): Dashboard {
  const models = zones.map(deriveDomain);
  const attention = models
    .flatMap((model) => model.attention)
    .sort((a, b) => {
      if (a.severity !== b.severity) {
        return a.severity === "warn" ? -1 : 1;
      }
      return a.zoneName.localeCompare(b.zoneName);
    });

  const warnZones = new Set(
    attention.filter((item) => item.severity === "warn").map((i) => i.zoneName),
  ).size;
  const emptyZones = attention.filter((item) => item.code === "empty").length;

  const total = models.length;
  const subtitle =
    total === 0
      ? "No domains yet"
      : `${total} ${plural(total, "domain")} · ${
          warnZones
            ? `${warnZones} need${warnZones === 1 ? "s" : ""} attention`
            : emptyZones
              ? `${emptyZones} not set up yet`
              : "nothing needs attention"
        }`;

  let latestModel: DomainModel | undefined;
  for (const model of models) {
    if (!model.updatedAt) {
      continue;
    }
    if (
      !latestModel?.updatedAt ||
      model.updatedAt.getTime() > latestModel.updatedAt.getTime()
    ) {
      latestModel = model;
    }
  }

  return {
    rows: models.map(toDomainRow),
    subtitle,
    attention,
    latest:
      latestModel && latestModel.updatedAt
        ? {
            zoneName: latestModel.zone.name,
            serial: latestModel.serial,
            updatedAt: latestModel.updatedAt,
            recordCount: latestModel.zone.records.length,
          }
        : undefined,
    server: status
      ? {
          queries: status.queriesTotal,
          zones: status.zoneCount,
          records: status.recordCount,
          startedAt: toDate(status.startedAt),
        }
      : undefined,
  };
}
