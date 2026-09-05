/**
 * Mail-provider presets for "Route email".
 *
 * These are published setup values, not a claim that a provider is in use: every field
 * is editable before anything is written, and the form says so. Nothing here promises
 * delivery or spam outcomes — DNS records alone decide neither.
 */

export type MailPreset = {
  id: string;
  label: string;
  mx: Array<{ priority: number; host: string }>;
  spfInclude?: string;
  dkim?: (zone: string) => Array<{ selector: string; target: string }>;
  needsToken?: {
    label: string;
    placeholder: string;
    apply: (token: string) => Array<{ priority: number; host: string }>;
    /** Turns what the provider's admin page displays into the bare token. */
    normalise?: (raw: string) => string;
    /** Rejects a token that would build a host the provider does not serve. */
    problem?: (token: string) => string | undefined;
  };
  hint: string;
};

const outlookSuffix = ".mail.protection.outlook.com";

export const mailPresets: MailPreset[] = [
  {
    id: "google",
    label: "Google Workspace",
    mx: [{ priority: 1, host: "smtp.google.com" }],
    spfInclude: "_spf.google.com",
    hint: "Google's current single-record MX setup.",
  },
  {
    id: "microsoft",
    label: "Microsoft 365",
    mx: [],
    spfInclude: "spf.protection.outlook.com",
    needsToken: {
      label: "MX token from the Microsoft 365 admin center (e.g. contoso-com)",
      placeholder: "contoso-com",
      apply: (token) => [{ priority: 0, host: `${token}${outlookSuffix}` }],
      // The admin center shows the whole MX host, so that is what people paste;
      // appending the suffix to it would publish contoso-com.mail.protection.
      // outlook.com.mail.protection.outlook.com, which the server accepts.
      normalise: (raw) => {
        const value = raw.trim().replace(/\.+$/, "");
        return value.toLowerCase().endsWith(outlookSuffix)
          ? value.slice(0, value.length - outlookSuffix.length)
          : value;
      },
      problem: (token) =>
        /^[a-z0-9-]+$/i.test(token)
          ? undefined
          : `The MX token is the part before ${outlookSuffix} (e.g. contoso-com).`,
    },
    hint: "The MX host is tenant-specific; copy it from Settings → Domains.",
  },
  {
    id: "fastmail",
    label: "Fastmail",
    mx: [
      { priority: 10, host: "in1-smtp.messagingengine.com" },
      { priority: 20, host: "in2-smtp.messagingengine.com" },
    ],
    spfInclude: "spf.messagingengine.com",
    dkim: (zone) =>
      [1, 2, 3].map((n) => ({
        selector: `fm${n}`,
        target: `fm${n}.${zone}.dkim.fmhosted.com`,
      })),
    hint: "Fastmail publishes DKIM through three CNAMEs.",
  },
  {
    id: "icloud",
    label: "iCloud Mail",
    mx: [
      { priority: 10, host: "mx01.mail.icloud.com" },
      { priority: 10, host: "mx02.mail.icloud.com" },
    ],
    spfInclude: "icloud.com",
    dkim: (zone) => [
      { selector: "sig1", target: `sig1.dkim.${zone}.at.icloudmailadmin.com` },
    ],
    hint: "Values from iCloud Mail custom-domain setup.",
  },
  {
    id: "proton",
    label: "Proton Mail",
    mx: [
      { priority: 10, host: "mail.protonmail.ch" },
      { priority: 20, host: "mailsec.protonmail.ch" },
    ],
    spfInclude: "_spf.protonmail.ch",
    hint: "Proton's DKIM CNAMEs are unique per domain — add them with Add DKIM key.",
  },
  {
    id: "custom",
    label: "Custom",
    mx: [],
    hint: "Enter the mail servers and SPF include your provider gave you.",
  },
  {
    id: "none",
    label: "No mail for this domain",
    mx: [{ priority: 0, host: "." }],
    hint: "Publishes a null MX, SPF -all and a reject DMARC policy, which tell receivers to refuse mail claiming to come from this domain.",
  },
];

export const defaultMailPresetId = "google";

export function mailPreset(id: string): MailPreset {
  return (
    mailPresets.find((preset) => preset.id === id) ??
    mailPresets[0]
  );
}

/** The bare token behind whatever the user pasted, or "" for a preset without one. */
export function mailPresetToken(preset: MailPreset, raw: string): string {
  const needed = preset.needsToken;
  if (!needed) {
    return "";
  }
  return needed.normalise ? needed.normalise(raw) : raw.trim();
}

/** Why the token cannot build a mail server host, if it cannot. */
export function mailPresetTokenProblem(
  preset: MailPreset,
  raw: string,
): string | undefined {
  const needed = preset.needsToken;
  const token = mailPresetToken(preset, raw);
  if (!needed || !token) {
    return undefined;
  }
  return needed.problem?.(token);
}

/** The SPF value a preset starts from; every preset's value stays editable. */
export function presetSpfValue(preset: MailPreset): string {
  if (preset.id === "none") {
    return "v=spf1 -all";
  }
  if (preset.spfInclude) {
    return `v=spf1 include:${preset.spfInclude} ~all`;
  }
  return "v=spf1 mx ~all";
}
