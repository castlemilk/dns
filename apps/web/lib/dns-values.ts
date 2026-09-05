import { RecordType } from "@/gen/dns/v1/dns_pb";

/**
 * Pure string rules that mirror the Go backend (internal/zone, internal/zonefile).
 * No React, no DOM: safe in route handlers, server components and client components.
 *
 * Regex rule for this file: tsconfig targets ES2017, so named capture groups are a
 * compile error (TS1503). Numbered groups only. Lookbehind assertions are accepted.
 */

export type RecordDraft = {
  name: string;
  type: RecordType;
  ttl: number;
  value: string;
};

const typeNames: Record<RecordType, string> = {
  [RecordType.UNSPECIFIED]: "—",
  [RecordType.A]: "A",
  [RecordType.AAAA]: "AAAA",
  [RecordType.CNAME]: "CNAME",
  [RecordType.MX]: "MX",
  [RecordType.TXT]: "TXT",
  [RecordType.NS]: "NS",
  [RecordType.SRV]: "SRV",
  [RecordType.CAA]: "CAA",
  [RecordType.SOA]: "SOA",
};

export function recordTypeName(type: RecordType): string {
  return typeNames[type] ?? "UNKNOWN";
}

export const stripDot = (s: string) => (s.endsWith(".") ? s.slice(0, -1) : s);

export const lower = (s: string) => s.trim().toLowerCase();

export const sameHost = (a: string, b: string) =>
  lower(stripDot(a)) === lower(stripDot(b));

/** Display helper only: never sent to the API. */
export const fqdn = (zoneName: string, name: string) =>
  name === "@" ? zoneName : `${name}.${zoneName}`;

/** Mirrors the server's normalizeOwner for names that are already inside the zone. */
export function toRelativeName(zoneName: string, input: string): string {
  const zone = lower(zoneName);
  const t = stripDot(lower(input));
  if (t === "" || t === "@" || t === zone) {
    return "@";
  }
  if (t.endsWith(`.${zone}`)) {
    return t.slice(0, -(zone.length + 1));
  }
  return t;
}

/**
 * Mirrors the server's "must be inside the zone" check. Only meaningful when the raw
 * input ends with a dot; relative inputs are always inside the zone.
 */
export function isInsideZone(zoneName: string, input: string): boolean {
  const raw = input.trim();
  if (!raw.endsWith(".")) {
    return true;
  }
  const zone = lower(zoneName);
  const t = stripDot(lower(raw));
  return t === zone || t.endsWith(`.${zone}`);
}

/**
 * The owner name to send with a record write. Relative and in-zone inputs are relativised
 * (`www.acme.dev.` → `www`), but a fully-qualified name OUTSIDE the zone is passed through
 * untouched: stripping its trailing dot would turn `www.other.com.` — which the server
 * rejects with "must be inside the zone" — into the relative label `www.other.com`, quietly
 * published as www.other.com.acme.dev. Keeping the dot lets the server reject it.
 */
export function toOwnerName(zoneName: string, input: string): string {
  const raw = input.trim();
  return raw.endsWith(".") && !isInsideZone(zoneName, raw)
    ? raw
    : toRelativeName(zoneName, raw);
}

/**
 * Mirrors Go normalizeTarget: always returns a value with a trailing dot, expanding a
 * bare single label inside the zone. "." (the null-MX target) stays ".".
 */
export function normaliseTarget(zoneName: string, raw: string): string {
  const v = lower(raw);
  if (v === "@") {
    return `${lower(zoneName)}.`;
  }
  if (!v.endsWith(".") && !v.includes(".")) {
    return `${v}.${lower(zoneName)}.`;
  }
  return `${stripDot(v)}.`;
}

/**
 * RFC 1035 character-string decoding that mirrors miekg/dns packTxtString — i.e. what
 * the authoritative server actually serves, not Go's strconv.Quote escapes.
 */
export function unquoteTxt(stored: string): string {
  const s = stored.trim();
  if (!s.startsWith('"')) {
    return s;
  }
  const encoder = new TextEncoder();
  const decoder = new TextDecoder("utf-8");
  const pushChar = (bytes: number[], char: string) => {
    for (const byte of encoder.encode(char)) {
      bytes.push(byte);
    }
  };
  const charAt = (index: number) => {
    const cp = s.codePointAt(index);
    return cp === undefined ? "" : String.fromCodePoint(cp);
  };

  let out = "";
  let i = 0;
  while (i < s.length) {
    if (s[i] !== '"') {
      i += 1;
      continue;
    }
    i += 1;
    const bytes: number[] = [];
    while (i < s.length && s[i] !== '"') {
      if (s[i] === "\\") {
        const digits = s.slice(i + 1, i + 4);
        if (/^[0-9]{3}$/.test(digits)) {
          bytes.push(Number(digits) & 0xff);
          i += 4;
          continue;
        }
        const escaped = charAt(i + 1);
        if (escaped === "") {
          i += 1;
          continue;
        }
        pushChar(bytes, escaped);
        i += 1 + escaped.length;
        continue;
      }
      const char = charAt(i);
      pushChar(bytes, char);
      i += char.length;
    }
    i += 1;
    out += decoder.decode(new Uint8Array(bytes));
  }
  return out;
}

/**
 * Mirrors Go's `unicode.IsPrint` (letters, marks, numbers, punctuation, symbols and
 * U+0020), which is what `strconv.Quote` uses to decide whether to escape a rune.
 * Built with `new RegExp` because the ES2017 target rejects a `\p{…}` regex literal.
 */
const nonPrintableTxt = new RegExp("[\\p{C}\\p{Z}]", "u");

/**
 * TXT values the wire can carry verbatim. Anything else (control characters, line breaks,
 * and the invisible paste artefacts that ride along with copied DKIM keys and verification
 * tokens — zero-width space, soft hyphen, BOM, word joiner, bidi controls, non-ASCII
 * spaces) would be stored as a Go escape and served as the literal letters of that escape,
 * so it is rejected client-side.
 */
export function isPrintableTxt(text: string): boolean {
  for (const char of text) {
    if (char !== " " && nonPrintableTxt.test(char)) {
      return false;
    }
  }
  return true;
}

export type Mx = {
  priority: number;
  /** Lowercased, trailing dot stripped; "" for the null MX ("0 ."). */
  host: string;
};

export function parseMx(value: string): Mx | undefined {
  const match = /^([0-9]+)\s+(\S+)$/.exec(value.trim());
  if (!match) {
    return undefined;
  }
  const priority = Number(match[1]);
  if (!Number.isFinite(priority)) {
    return undefined;
  }
  return { priority, host: stripDot(lower(match[2])) };
}

export type Spf = {
  raw: string;
  /** Every term after v=spf1 except the `all` term and modifiers. */
  mechanisms: string[];
  includes: string[];
  all?: "-all" | "~all" | "?all" | "+all";
  redirect?: string;
};

const spfQualifiers = ["+", "-", "~", "?"];

export function parseSpf(text: string): Spf | undefined {
  const raw = text.trim();
  const terms = raw.split(/\s+/).filter((term) => term.length > 0);
  if (terms.length === 0 || !/^v=spf1$/i.test(terms[0])) {
    return undefined;
  }
  const mechanisms: string[] = [];
  const includes: string[] = [];
  let all: Spf["all"] | undefined;
  let redirect: string | undefined;

  for (const term of terms.slice(1)) {
    const qualifier = spfQualifiers.includes(term[0]) ? term[0] : undefined;
    const body = qualifier ? term.slice(1) : term;
    if (/^all$/i.test(body)) {
      all = `${qualifier ?? "+"}all` as Spf["all"];
      continue;
    }
    // Modifiers are `name=value` and never carry a qualifier; mechanisms use ":" or "/".
    if (!qualifier && /^[a-z][a-z0-9_.-]*=/i.test(term)) {
      const modifier = /^([a-z][a-z0-9_.-]*)=(.*)$/i.exec(term);
      if (modifier && lower(modifier[1]) === "redirect") {
        redirect = lower(modifier[2]);
      }
      continue;
    }
    mechanisms.push(term);
    const include = /^include:(.+)$/i.exec(body);
    if (include) {
      includes.push(lower(include[1]));
    }
  }

  return { raw, mechanisms, includes, all, redirect };
}

export type Dmarc = {
  valid: boolean;
  policy?: "none" | "quarantine" | "reject";
  rua?: string;
};

export function parseDmarc(text: string): Dmarc {
  const tags = text
    .split(";")
    .map((tag) => tag.trim())
    .filter((tag) => tag.length > 0);
  const values = new Map<string, string>();
  for (const tag of tags) {
    const eq = tag.indexOf("=");
    if (eq <= 0) {
      continue;
    }
    const key = lower(tag.slice(0, eq));
    if (!values.has(key)) {
      values.set(key, tag.slice(eq + 1).trim());
    }
  }
  const version = values.get("v");
  const rawPolicy = values.get("p");
  const policy =
    rawPolicy && /^(none|quarantine|reject)$/i.test(rawPolicy)
      ? (lower(rawPolicy) as Dmarc["policy"])
      : undefined;
  const valid =
    version !== undefined && /^dmarc1$/i.test(version) && policy !== undefined;
  return { valid, policy, rua: values.get("rua") };
}

export const isSpfText = (t: string) => /^v=spf1(\s|$)/i.test(t.trim());

export const isDkimName = (name: string) =>
  name === "_domainkey" || name.endsWith("._domainkey");

export const dkimSelector = (name: string) => name.replace(/\._domainkey$/, "");

export const isIPv4 = (s: string) =>
  /^(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])){3}$/.test(
    s.trim(),
  );

/** The server is authoritative for IPv6 validity; this is only a shape check. */
export const looksIPv6 = (s: string) =>
  s.includes(":") && /^[0-9a-f:.]+$/i.test(s.trim());

function expandIPv6Side(side: string, allowIPv4Tail: boolean): string[] | undefined {
  if (side === "") {
    return [];
  }
  const parts = side.split(":");
  const groups: string[] = [];
  for (let index = 0; index < parts.length; index += 1) {
    const part = parts[index];
    if (part === "") {
      return undefined;
    }
    if (part.includes(".")) {
      if (!allowIPv4Tail || index !== parts.length - 1 || !isIPv4(part)) {
        return undefined;
      }
      const octets = part.split(".").map(Number);
      groups.push(((octets[0] << 8) | octets[1]).toString(16));
      groups.push(((octets[2] << 8) | octets[3]).toString(16));
      continue;
    }
    if (!/^[0-9a-f]{1,4}$/.test(part)) {
      return undefined;
    }
    groups.push(parseInt(part, 16).toString(16));
  }
  return groups;
}

/** Mirrors netip.Addr.String() closely enough for the comparisons this UI makes. */
export function canonicalIPv6(s: string): string | undefined {
  const v = lower(s);
  if (v === "" || !v.includes(":")) {
    return undefined;
  }
  const sides = v.split("::");
  if (sides.length > 2) {
    return undefined;
  }

  let groups: string[];
  if (sides.length === 2) {
    const head = expandIPv6Side(sides[0], false);
    const tail = expandIPv6Side(sides[1], true);
    if (!head || !tail) {
      return undefined;
    }
    const missing = 8 - head.length - tail.length;
    if (missing < 1) {
      return undefined;
    }
    groups = [...head, ...new Array<string>(missing).fill("0"), ...tail];
  } else {
    const only = expandIPv6Side(sides[0], true);
    if (!only || only.length !== 8) {
      return undefined;
    }
    groups = only;
  }

  let bestStart = -1;
  let bestLength = 0;
  let runStart = -1;
  let runLength = 0;
  for (let index = 0; index < groups.length; index += 1) {
    if (groups[index] === "0") {
      if (runStart < 0) {
        runStart = index;
        runLength = 0;
      }
      runLength += 1;
      if (runLength > bestLength) {
        bestLength = runLength;
        bestStart = runStart;
      }
    } else {
      runStart = -1;
      runLength = 0;
    }
  }
  if (bestLength < 2) {
    return groups.join(":");
  }
  const head = groups.slice(0, bestStart).join(":");
  const tail = groups.slice(bestStart + bestLength).join(":");
  return `${head}::${tail}`;
}

export const sameIp = (a: string, b: string) =>
  a.trim() === b.trim() ||
  (canonicalIPv6(a) !== undefined && canonicalIPv6(a) === canonicalIPv6(b));

export const hostnamePattern =
  /^(?=.{3,253}$)(?!-)[a-z0-9-]{1,63}(?<!-)(\.(?!-)[a-z0-9-]{1,63}(?<!-))+$/;

export function isHostname(s: string): boolean {
  const d = stripDot(lower(s));
  if (!hostnamePattern.test(d)) {
    return false;
  }
  const labels = d.split(".");
  return !/^[0-9]+$/.test(labels[labels.length - 1]);
}

export function normaliseZoneName(input: string): string {
  let value = lower(input).replace(/^https?:\/\//, "");
  const slash = value.indexOf("/");
  if (slash >= 0) {
    value = value.slice(0, slash);
  }
  return stripDot(value.trim());
}

/**
 * Next already decodes dynamic segments, so a second decodeURIComponent throws a
 * URIError on a literal "%" (e.g. /domains/50%25 arrives as "50%").
 */
export function normaliseZoneParam(p: string): string {
  try {
    return normaliseZoneName(decodeURIComponent(p));
  } catch {
    return normaliseZoneName(p);
  }
}

/**
 * The backend only rejects whitespace, control characters and `; $ ( ) \ "` in a zone
 * name, so `a?b.com`, `evil.com/x.victim.com` and `user:pw@evil.com` are all storable.
 * Interpolating those raw into a URL changes its meaning (query, fragment, path, userinfo),
 * so every link to a zone goes through these helpers. `normaliseZoneParam` decodes the
 * segment again, so encoded names round-trip.
 */
export const zoneHref = (zoneName: string) =>
  `/domains/${encodeURIComponent(zoneName)}`;

export const connectHref = (zoneName: string) =>
  `/connect?domain=${encodeURIComponent(zoneName)}`;
