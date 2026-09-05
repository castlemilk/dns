import { Code, ConnectError } from "@connectrpc/connect";

import {
  RecordType,
  type Record as DNSRecord,
  type Zone,
} from "@/gen/dns/v1/dns_pb";
import type { ZonesApi } from "@/components/app/zones-provider";
import {
  isInsideZone,
  isSpfText,
  lower,
  normaliseTarget,
  parseDmarc,
  recordTypeName,
  sameHost,
  sameIp,
  toRelativeName,
  unquoteTxt,
  type RecordDraft,
} from "@/lib/dns-values";
import { describeError } from "@/lib/errors";
import { recordOrigin, type RecordOrigin } from "@/lib/zone-model";

/**
 * Record planners for the guided flows. Pure: every function reads a Zone and returns
 * the operations that would converge it, so a form can render the plan live before a
 * single RPC is made. `applyPlan` is the only function that touches the network, and it
 * goes through the provider's mutations so state stays server-authoritative.
 *
 * Backend rules honoured here (verified in internal/zone):
 *  - every record in an RRset must share one TTL (`ttlFor` + `alignTtl`);
 *  - a CNAME cannot coexist with another record of the same name (deletes are ordered
 *    before creates by `orderOps`);
 *  - the apex is never a CNAME;
 *  - TXT values are sent unquoted (the server quotes them) and compared unquoted;
 *  - CNAME/MX/NS targets are sent through `normaliseTarget` so a bare single label is
 *    not silently expanded inside the zone by the server;
 *  - records written by the hosting or mail engine are fixed: the reconcilers rewrite
 *    them within one pass and the control plane refuses hand edits (§3.2), so a planner
 *    never emits an update or delete on one — it says so in a note instead.
 */

export type RecordOp =
  | { op: "create"; draft: RecordDraft; why: string }
  | {
      op: "update";
      recordId: string;
      before: DNSRecord;
      draft: RecordDraft;
      why: string;
    }
  | { op: "delete"; recordId: string; before: DNSRecord; why: string };

export type RecordPlan = { ops: RecordOp[]; notes: string[] };

/** One rendered line: `"A @ → 203.0.113.10"` with an optional `"(TTL 0 → 300)"`. */
export type PlanLine = {
  kind: "create" | "update" | "delete";
  text: string;
  detail?: string;
};

/** The provider mutations `applyPlan` needs. */
export type RecordWriteApi = Pick<
  ZonesApi,
  "createRecord" | "updateRecord" | "deleteRecord"
>;

export const defaultTtl = 300;

const alignWhy = "align TTL for the RRset";

/** Records an engine owns; `undefined` for the operator's own records. */
function engineOf(record: DNSRecord): Exclude<RecordOrigin, "user"> | undefined {
  const origin = recordOrigin(record);
  return origin === "user" ? undefined : origin;
}

export function isEngineOwned(record: DNSRecord): boolean {
  return engineOf(record) !== undefined;
}

function engineNote(record: DNSRecord): string {
  const type = recordTypeName(record.type);
  return engineOf(record) === "hosting"
    ? `${type} ${record.name} is written by the Website engine — detach the site to change it`
    : `${type} ${record.name} is written by the Email engine — unbind email to change it`;
}

/**
 * True when the RRset holds an engine record, in which case the planner leaves the
 * whole RRset alone and explains why: writing a second value beside the engine's would
 * be applied and then reverted by the next reconciler pass.
 */
function blockedByEngine(
  records: DNSRecord[],
  notes: string[],
): boolean {
  const owned = records.filter(isEngineOwned);
  if (!owned.length) {
    return false;
  }
  for (const record of owned) {
    const note = engineNote(record);
    if (!notes.includes(note)) {
      notes.push(note);
    }
  }
  return true;
}

export const isEmailAddress = (s: string) =>
  /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(s.trim());

function userRecords(zone: Zone): DNSRecord[] {
  return zone.records.filter((record) => !record.managed);
}

function recordsAt(zone: Zone, name: string, type?: RecordType): DNSRecord[] {
  const owner = lower(name);
  return userRecords(zone).filter(
    (record) =>
      lower(record.name) === owner && (type === undefined || record.type === type),
  );
}

/**
 * The TTL a write into (name, type) must use so the whole RRset keeps one TTL.
 * TTL 0 only survives an import (CRUD coerces 0 → 300), and writing 300 next to a
 * surviving TTL-0 sibling is rejected, so 0 maps to the fallback and `alignTtl`
 * converges the rest of the RRset.
 */
export function ttlFor(
  zone: Zone,
  name: string,
  type: RecordType,
  fallback = defaultTtl,
): number {
  const ttl = recordsAt(zone, name, type)[0]?.ttl;
  return ttl === undefined || ttl === 0 ? fallback : ttl;
}

function opTouches(op: RecordOp, recordId: string): boolean {
  return op.op !== "create" && op.recordId === recordId;
}

/**
 * Converges a TTL-0 RRset to `defaultTtl` before anything is written into it.
 *
 * An update of one member is validated against every remaining sibling, so with two
 * members still at TTL 0 the first update would be rejected. Only the first untouched
 * member is updated in place; the rest are deleted and re-created, which — with
 * `orderOps` running deletes first — empties the RRset down to the one record whose
 * update then sees no siblings. Re-created records get new ids.
 */
export function alignTtl(
  zone: Zone,
  ops: RecordOp[],
  name: string,
  type: RecordType,
): void {
  // Engine records are never rewritten here; they always carry the engine's own TTL.
  const existing = recordsAt(zone, name, type).filter(
    (record) => !isEngineOwned(record),
  );
  if (!existing.some((record) => record.ttl === 0)) {
    return;
  }

  const untouched = existing.filter(
    (record) => !ops.some((op) => opTouches(op, record.id)),
  );
  const hasUpdate = ops.some(
    (op) =>
      op.op === "update" &&
      lower(op.before.name) === lower(name) &&
      op.before.type === type,
  );

  untouched.forEach((record, index) => {
    const draft: RecordDraft = {
      name: lower(name),
      type,
      ttl: defaultTtl,
      value: record.value,
    };
    if (!hasUpdate && index === 0) {
      ops.push({
        op: "update",
        recordId: record.id,
        before: record,
        draft,
        why: alignWhy,
      });
      return;
    }
    ops.push({ op: "delete", recordId: record.id, before: record, why: alignWhy });
    ops.push({ op: "create", draft, why: alignWhy });
  });
}

/** Compares a stored value with a value about to be written, per record type. */
function sameValue(
  zoneName: string,
  type: RecordType,
  stored: string,
  wanted: string,
): boolean {
  switch (type) {
    case RecordType.A:
    case RecordType.AAAA:
      return sameIp(stored, wanted);
    case RecordType.CNAME:
    case RecordType.NS:
      return (
        normaliseTarget(zoneName, stored) === normaliseTarget(zoneName, wanted)
      );
    case RecordType.MX:
      return normaliseMx(zoneName, stored) === normaliseMx(zoneName, wanted);
    case RecordType.TXT:
      return unquoteTxt(stored) === unquoteTxt(wanted);
    default:
      return lower(stored) === lower(wanted);
  }
}

/** `"10 mx1.example.net"` → `"10 mx1.example.net."`; the null MX `"0 ."` is kept. */
function normaliseMx(zoneName: string, value: string): string {
  const match = /^([0-9]+)\s+(\S+)$/.exec(value.trim());
  if (!match) {
    return lower(value);
  }
  return `${Number(match[1])} ${normaliseTarget(zoneName, match[2])}`;
}

export const mxValue = (zoneName: string, priority: number, host: string) =>
  `${priority} ${normaliseTarget(zoneName, host)}`;

/**
 * Converges (name, type) to exactly `value`. `undefined` leaves the RRset alone, which
 * is how a blank optional field is expressed.
 */
export function setSingle(
  zone: Zone,
  ops: RecordOp[],
  name: string,
  type: RecordType,
  value: string | undefined,
  why: string,
  notes: string[] = [],
): void {
  if (value === undefined) {
    return;
  }
  const owner = lower(name);
  const existing = recordsAt(zone, owner, type);
  if (blockedByEngine(existing, notes)) {
    return;
  }
  const ttl = ttlFor(zone, owner, type);
  const duplicateWhy = `duplicate ${recordTypeName(type)} at ${owner}`;
  const keep = existing.find((record) =>
    sameValue(zone.name, type, record.value, value),
  );

  if (keep) {
    for (const record of existing) {
      if (record.id !== keep.id) {
        ops.push({
          op: "delete",
          recordId: record.id,
          before: record,
          why: duplicateWhy,
        });
      }
    }
    return;
  }

  const draft: RecordDraft = { name: owner, type, ttl, value };
  if (existing.length > 0) {
    ops.push({
      op: "update",
      recordId: existing[0].id,
      before: existing[0],
      draft,
      why,
    });
    for (const record of existing.slice(1)) {
      ops.push({
        op: "delete",
        recordId: record.id,
        before: record,
        why: duplicateWhy,
      });
    }
    alignTtl(zone, ops, owner, type);
    return;
  }

  alignTtl(zone, ops, owner, type);
  ops.push({ op: "create", draft, why });
}

const opRank = { delete: 0, update: 1, create: 2 } as const;

/**
 * Deletes → updates → creates. Array.prototype.sort is stable, so records keep their
 * planned order inside each phase. This ordering is what lets a CNAME replace an A
 * record of the same name, and what lets a TTL-0 RRset converge (see `alignTtl`).
 */
export function orderOps(ops: RecordOp[]): RecordOp[] {
  return [...ops].sort((a, b) => opRank[a.op] - opRank[b.op]);
}

function ttlDetail(from: number, to: number): string | undefined {
  return from === to ? undefined : `(TTL ${from} → ${to})`;
}

export function describePlan(plan: RecordPlan): PlanLine[] {
  const ordered = orderOps(plan.ops);
  // A delete+create pair written by `alignTtl` re-publishes the same value at a new
  // TTL; pairing them up lets the created line show the TTL change too.
  const deleted = new Map<string, DNSRecord>();
  for (const op of ordered) {
    if (op.op === "delete") {
      const key = `${lower(op.before.name)}|${op.before.type}|${op.before.value}`;
      if (!deleted.has(key)) {
        deleted.set(key, op.before);
      }
    }
  }

  return ordered.map((op) => {
    if (op.op === "delete") {
      const type = recordTypeName(op.before.type);
      return {
        kind: "delete" as const,
        text: `${type} ${op.before.name} → ${op.before.value}`,
        detail: op.why,
      };
    }
    if (op.op === "update") {
      const type = recordTypeName(op.draft.type);
      return {
        kind: "update" as const,
        text: `${type} ${op.draft.name}: ${op.before.value} → ${op.draft.value}`,
        detail: ttlDetail(op.before.ttl, op.draft.ttl) ?? op.why,
      };
    }
    const type = recordTypeName(op.draft.type);
    const twin = deleted.get(
      `${lower(op.draft.name)}|${op.draft.type}|${op.draft.value}`,
    );
    return {
      kind: "create" as const,
      text: `${type} ${op.draft.name} → ${op.draft.value}`,
      detail: (twin && ttlDetail(twin.ttl, op.draft.ttl)) ?? op.why,
    };
  });
}

export type ApplyPlanResult = {
  applied: number;
  total: number;
  /** `code` is the Connect status of the first failure, e.g. `Code.AlreadyExists`. */
  failed?: { op: RecordOp; message: string; code: Code };
};

/**
 * Runs the plan sequentially through the provider. Never throws: it stops at the first
 * failure and reports what was written, because a half-applied plan is still real state
 * the domain view must show.
 */
export async function applyPlan(
  api: RecordWriteApi,
  zoneId: string,
  plan: RecordPlan,
  onProgress?: (done: number, total: number) => void,
): Promise<ApplyPlanResult> {
  const ops = orderOps(plan.ops);
  let applied = 0;

  // Belt and braces: the planners never produce such an op, and the control plane
  // refuses it anyway (§3.2). Failing here means nothing is half-written first.
  const engineOp = ops.find(
    (op) => op.op !== "create" && isEngineOwned(op.before),
  );
  if (engineOp && engineOp.op !== "create") {
    return {
      applied: 0,
      total: ops.length,
      failed: {
        op: engineOp,
        message: engineNote(engineOp.before),
        code: Code.FailedPrecondition,
      },
    };
  }

  for (const op of ops) {
    try {
      if (op.op === "delete") {
        await api.deleteRecord(zoneId, op.recordId);
      } else if (op.op === "update") {
        await api.updateRecord(zoneId, op.recordId, op.draft);
      } else {
        await api.createRecord(zoneId, op.draft);
      }
    } catch (error) {
      return {
        applied,
        total: ops.length,
        failed: {
          op,
          message: describeError(error),
          code: ConnectError.from(error).code,
        },
      };
    }
    applied += 1;
    onProgress?.(applied, ops.length);
  }
  return { applied, total: ops.length };
}

/**
 * `applyPlan` plus the zone the last successful write returned. Forms need it as the
 * completion signal for `onApplied`, and reading it back from the provider would race
 * the render that publishes the new state.
 */
export async function applyPlanToZone(
  api: RecordWriteApi,
  zoneId: string,
  plan: RecordPlan,
  onProgress?: (done: number, total: number) => void,
): Promise<{ result: ApplyPlanResult; zone?: Zone }> {
  let zone: Zone | undefined;
  const capture = (next: Zone) => {
    zone = next;
    return next;
  };
  const result = await applyPlan(
    {
      createRecord: (id, draft) => api.createRecord(id, draft).then(capture),
      updateRecord: (id, recordId, draft) =>
        api.updateRecord(id, recordId, draft).then(capture),
      deleteRecord: (id, recordId) => api.deleteRecord(id, recordId).then(capture),
    },
    zoneId,
    plan,
    onProgress,
  );
  return { result, zone };
}

/** Splits a server validation message such as `"value: enter a valid IPv4 address"`. */
export function splitFieldError(
  message: string,
): { field?: "name" | "type" | "ttl" | "value"; message: string } {
  const match = /^(name|type|ttl|value):\s*(.+)$/.exec(message);
  if (!match) {
    return { message };
  }
  return { field: match[1] as "name" | "type" | "ttl" | "value", message: match[2] };
}

const spfQualifier = /^[+\-~?]?/;

/** Inserts `include:<target>` before the `all` term, or appends it when there is none. */
export function mergeSpfInclude(existing: string, include: string): string {
  const text = existing.trim();
  const terms = text.split(/\s+/).filter((term) => term.length > 0);
  const target = lower(include);
  const already = terms.some((term) => {
    const body = term.replace(spfQualifier, "");
    const match = /^include:(.+)$/i.exec(body);
    return match !== null && lower(match[1]) === target;
  });
  if (already) {
    return text;
  }
  const term = `include:${include}`;
  const allIndex = terms.findIndex((candidate) =>
    /^[+\-~?]?all$/i.test(candidate),
  );
  if (allIndex < 0) {
    return [...terms, term].join(" ");
  }
  return [...terms.slice(0, allIndex), term, ...terms.slice(allIndex)].join(" ");
}

/* ------------------------------------------------------------------ website */

export type WebsiteInput = {
  mode: "ip" | "host";
  ipv4?: string;
  ipv6?: string;
  target?: string;
  www: boolean;
};

const pointingTypes = [RecordType.A, RecordType.AAAA, RecordType.CNAME];

/**
 * The one input combination that produces records which cannot resolve: a `www` alias
 * of an apex with no address at all (§E.3's "dangling" case).
 */
export function validateWebsiteInput(
  input: WebsiteInput,
): string | undefined {
  const ipv4 = input.ipv4?.trim();
  const ipv6 = input.ipv6?.trim();
  if (input.mode === "ip" && !ipv4 && !ipv6) {
    return "Enter at least one address.";
  }
  if (input.mode === "host" && !input.target?.trim()) {
    return "Enter the host name your provider gave you.";
  }
  return undefined;
}

export function planWebsite(zone: Zone, input: WebsiteInput): RecordPlan {
  const ops: RecordOp[] = [];
  const notes: string[] = [];
  const ipv4 = input.ipv4?.trim() || undefined;
  const ipv6 = input.ipv6?.trim() || undefined;

  setSingle(zone, ops, "@", RecordType.A, ipv4, `point ${zone.name}`, notes);
  setSingle(
    zone,
    ops,
    "@",
    RecordType.AAAA,
    ipv6,
    `point ${zone.name} (IPv6)`,
    notes,
  );

  // A blank address field leaves that family's records untouched — say so, rather than
  // letting the preview's silence imply they were removed.
  if (!ipv4 && recordsAt(zone, "@", RecordType.A).length > 0) {
    notes.push(`Existing A at ${zone.name} is kept.`);
  }
  if (!ipv6 && recordsAt(zone, "@", RecordType.AAAA).length > 0) {
    notes.push(`Existing AAAA at ${zone.name} is kept.`);
  }

  const wwwTarget =
    input.mode === "host"
      ? normaliseTarget(zone.name, input.target?.trim() ?? "")
      : `${lower(zone.name)}.`;

  const wwwRecords = recordsAt(zone, "www");
  if ((input.mode === "host" || input.www) && blockedByEngine(wwwRecords, notes)) {
    // The hosting engine owns `www`; leave it to the reconciler.
  } else if (input.mode === "host" || input.www) {
    // A CNAME cannot share its name with another record, so anything else at www goes.
    for (const record of wwwRecords) {
      if (record.type !== RecordType.CNAME) {
        ops.push({
          op: "delete",
          recordId: record.id,
          before: record,
          why: "www becomes an alias",
        });
      }
    }
    const cname = recordsAt(zone, "www", RecordType.CNAME)[0];
    const draft: RecordDraft = {
      name: "www",
      type: RecordType.CNAME,
      ttl: ttlFor(zone, "www", RecordType.CNAME),
      value: wwwTarget,
    };
    if (cname && sameHost(cname.value, wwwTarget)) {
      // Already pointed where it should be.
    } else if (cname) {
      ops.push({
        op: "update",
        recordId: cname.id,
        before: cname,
        draft,
        why: `point www.${zone.name}`,
      });
    } else {
      ops.push({ op: "create", draft, why: `point www.${zone.name}` });
    }
  } else {
    notes.push(`www.${zone.name} is left as it is.`);
  }

  if (
    userRecords(zone).some(
      (record) => record.name === "*" && pointingTypes.includes(record.type),
    )
  ) {
    notes.push(`Wildcard *.${zone.name} is kept.`);
  }

  return { ops, notes };
}

/* -------------------------------------------------------------------- email */

export type EmailInput = {
  mx: Array<{ priority: number; host: string }>;
  spf?: { mode: "merge" | "replace"; include?: string; value: string };
  dmarc?: { policy: "none" | "quarantine" | "reject"; rua: string };
  dkim: Array<{ selector: string; target: string }>;
};

export function dmarcValue(policy: string, rua: string): string {
  const address = rua.trim();
  return address
    ? `v=DMARC1; p=${policy}; rua=mailto:${address}`
    : `v=DMARC1; p=${policy}`;
}

export function planEmail(zone: Zone, input: EmailInput): RecordPlan {
  const ops: RecordOp[] = [];
  const notes: string[] = [];

  /* MX: converge the apex MX RRset to exactly input.mx. */
  const existingMx = recordsAt(zone, "@", RecordType.MX);
  const mxBlocked = blockedByEngine(existingMx, notes);
  const mxTtl = ttlFor(zone, "@", RecordType.MX);
  // De-duplicated: two rows naming the same server (the same host at the same
  // priority, however it was typed) are one MX record, and planning the create twice
  // would half-apply the plan — the server rejects the second with "already exists".
  const want = [
    ...new Set(
      input.mx.map((entry) => mxValue(zone.name, entry.priority, entry.host)),
    ),
  ];
  const wantSet = new Set(want);
  for (const record of mxBlocked ? [] : existingMx) {
    if (!wantSet.has(normaliseMx(zone.name, record.value))) {
      ops.push({
        op: "delete",
        recordId: record.id,
        before: record,
        why: "replace mail servers",
      });
    }
  }
  const missingMx = mxBlocked
    ? []
    : want.filter(
        (value) =>
          !existingMx.some(
            (record) => normaliseMx(zone.name, record.value) === value,
          ),
      );
  if (missingMx.length > 0) {
    alignTtl(zone, ops, "@", RecordType.MX);
    for (const value of missingMx) {
      ops.push({
        op: "create",
        draft: { name: "@", type: RecordType.MX, ttl: mxTtl, value },
        why: value.endsWith(" .")
          ? "null MX: rejects all mail"
          : "mail server",
      });
    }
  }

  /* SPF: at most one v=spf1 TXT at @. Verification TXTs at @ are never rewritten,
     except when alignTtl has to lift the whole RRset off TTL 0. */
  const apexTxt = recordsAt(zone, "@", RecordType.TXT);
  const spfRecords = apexTxt.filter((record) => isSpfText(unquoteTxt(record.value)));
  const txtTtl = ttlFor(zone, "@", RecordType.TXT);
  // Only complain about the engine's SPF when this plan actually wanted to write one.
  const spfBlocked = input.spf ? blockedByEngine(spfRecords, notes) : false;
  if (input.spf && !spfBlocked) {
    for (const record of spfRecords.slice(1)) {
      ops.push({
        op: "delete",
        recordId: record.id,
        before: record,
        why: "only one SPF record is allowed",
      });
    }
    const current = spfRecords[0];
    const value =
      current && input.spf.mode === "merge" && input.spf.include
        ? mergeSpfInclude(unquoteTxt(current.value), input.spf.include)
        : input.spf.value.trim();
    const draft: RecordDraft = {
      name: "@",
      type: RecordType.TXT,
      ttl: txtTtl,
      value,
    };
    if (current) {
      if (unquoteTxt(current.value) !== value) {
        ops.push({
          op: "update",
          recordId: current.id,
          before: current,
          draft,
          why: "spam protection (SPF)",
        });
        alignTtl(zone, ops, "@", RecordType.TXT);
      }
    } else {
      alignTtl(zone, ops, "@", RecordType.TXT);
      ops.push({ op: "create", draft, why: "spam protection (SPF)" });
    }
  }

  /* DMARC: created when absent, replaced only when invalid, never overwritten. */
  const dmarcRecord = recordsAt(zone, "_dmarc", RecordType.TXT)[0];
  const dmarcBlocked =
    input.dmarc && dmarcRecord ? blockedByEngine([dmarcRecord], notes) : false;
  if (input.dmarc && !dmarcBlocked) {
    const value = dmarcValue(input.dmarc.policy, input.dmarc.rua);
    const draft: RecordDraft = {
      name: "_dmarc",
      type: RecordType.TXT,
      ttl: ttlFor(zone, "_dmarc", RecordType.TXT),
      value,
    };
    if (!dmarcRecord) {
      alignTtl(zone, ops, "_dmarc", RecordType.TXT);
      ops.push({ op: "create", draft, why: "spam protection (DMARC)" });
    } else if (!parseDmarc(unquoteTxt(dmarcRecord.value)).valid) {
      ops.push({
        op: "update",
        recordId: dmarcRecord.id,
        before: dmarcRecord,
        draft,
        why: "replace an invalid DMARC record",
      });
      alignTtl(zone, ops, "_dmarc", RecordType.TXT);
    } else {
      notes.push("DMARC is already set and was left unchanged.");
    }
  }

  /* DKIM CNAMEs: a CNAME cannot share its name with anything else — including a
     second CNAME from another row with the same selector, which is why the rows are
     collapsed by selector first (last row wins) rather than planned one by one. */
  const dkimBySelector = new Map<string, { selector: string; target: string }>();
  for (const entry of input.dkim) {
    const selector = lower(entry.selector);
    if (selector) {
      dkimBySelector.set(selector, entry);
    }
  }
  for (const [selector, entry] of dkimBySelector) {
    const name = `${selector}._domainkey`;
    const target = normaliseTarget(zone.name, entry.target.trim());
    const atName = recordsAt(zone, name);
    if (blockedByEngine(atName, notes)) {
      continue;
    }
    for (const record of atName) {
      if (record.type !== RecordType.CNAME) {
        ops.push({
          op: "delete",
          recordId: record.id,
          before: record,
          why: "DKIM becomes an alias",
        });
      }
    }
    const cname = recordsAt(zone, name, RecordType.CNAME)[0];
    const draft: RecordDraft = {
      name,
      type: RecordType.CNAME,
      ttl: ttlFor(zone, name, RecordType.CNAME),
      value: target,
    };
    if (cname && sameHost(cname.value, target)) {
      continue;
    }
    if (cname) {
      ops.push({
        op: "update",
        recordId: cname.id,
        before: cname,
        draft,
        why: `DKIM key ${selector}`,
      });
      continue;
    }
    ops.push({ op: "create", draft, why: `DKIM key ${selector}` });
  }

  return { ops, notes };
}

/* ------------------------------------------------------------------- verify */

export type VerifyInput = { name: string; text: string };

/** Mirrors the server's "must be inside the zone" rule for fully-qualified names. */
export function verifyNameError(zone: Zone, name: string): string | undefined {
  return isInsideZone(zone.name, name) ? undefined : "must be inside the zone";
}

export function planVerify(zone: Zone, input: VerifyInput): RecordPlan {
  const ops: RecordOp[] = [];
  const notes: string[] = [];
  const text = input.text.trim();
  if (!text || verifyNameError(zone, input.name)) {
    return { ops, notes };
  }

  // The relative name is resolved once so the TTL lookup finds the RRset the record
  // will actually join (an input of "acme.dev." is the apex TXT set).
  const name = toRelativeName(zone.name, input.name);
  const existing = recordsAt(zone, name, RecordType.TXT);
  if (existing.some((record) => unquoteTxt(record.value) === text)) {
    notes.push("That record is already published.");
    return { ops, notes };
  }

  alignTtl(zone, ops, name, RecordType.TXT);
  ops.push({
    op: "create",
    draft: {
      name,
      type: RecordType.TXT,
      ttl: ttlFor(zone, name, RecordType.TXT),
      value: text,
    },
    why: "service verification",
  });
  return { ops, notes };
}
