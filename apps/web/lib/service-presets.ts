import { lower, stripDot } from "@/lib/dns-values";

/**
 * Deterministic readings of stored record values: no network, no fabricated data.
 * A match only labels what a value looks like, never who wrote the record.
 */

export type TxtPreset = {
  id: string;
  label: string;
  prefix: string;
  nameHint?: string;
};

export const txtPresets: TxtPreset[] = [
  {
    id: "google",
    label: "Google Search Console",
    prefix: "google-site-verification=",
  },
  { id: "microsoft", label: "Microsoft 365", prefix: "MS=" },
  { id: "apple", label: "Apple", prefix: "apple-domain-verification=" },
  { id: "meta", label: "Meta", prefix: "facebook-domain-verification=" },
  {
    id: "atlassian",
    label: "Atlassian",
    prefix: "atlassian-domain-verification=",
  },
  { id: "stripe", label: "Stripe", prefix: "stripe-verification=" },
  { id: "notion", label: "Notion", prefix: "notion-domain-verification=" },
];

export function recogniseTxt(text: string, name: string): string | undefined {
  const value = text.trim().toLowerCase();
  for (const preset of txtPresets) {
    if (value.startsWith(preset.prefix.toLowerCase())) {
      return preset.label;
    }
  }
  const owner = lower(name);
  if (owner === "_atproto") {
    return "Bluesky handle";
  }
  if (owner.startsWith("_github-challenge-")) {
    return "GitHub";
  }
  if (owner === "_acme-challenge") {
    return "ACME certificate challenge";
  }
  return undefined;
}

export const hostSuffixes: Array<[suffix: string, label: string]> = [
  [".fly.dev", "Fly.io"],
  [".netlify.app", "Netlify"],
  [".vercel-dns.com", "Vercel"],
  [".vercel.app", "Vercel"],
  [".github.io", "GitHub Pages"],
  [".pages.dev", "Cloudflare Pages"],
  [".herokudns.com", "Heroku"],
  [".onrender.com", "Render"],
  [".myshopify.com", "Shopify"],
  [".ghost.io", "Ghost"],
  [".webflow.io", "Webflow"],
  [".squarespace.com", "Squarespace"],
];

export function recogniseHost(target: string): string | undefined {
  const host = stripDot(lower(target));
  for (const [suffix, label] of hostSuffixes) {
    if (host.endsWith(suffix)) {
      return label;
    }
  }
  return undefined;
}

export const mailSuffixes: Array<[suffix: string, label: string]> = [
  [".google.com", "Google Workspace"],
  [".googlemail.com", "Google Workspace"],
  [".mail.protection.outlook.com", "Microsoft 365"],
  [".messagingengine.com", "Fastmail"],
  [".mail.icloud.com", "iCloud Mail"],
  [".protonmail.ch", "Proton"],
  [".zoho.com", "Zoho"],
  [".zoho.eu", "Zoho"],
  [".mxroute.com", "MXroute"],
  [".migadu.com", "Migadu"],
];

export function guessMailProvider(hosts: string[]): string | undefined {
  for (const raw of hosts) {
    const host = stripDot(lower(raw));
    for (const [suffix, label] of mailSuffixes) {
      if (host.endsWith(suffix)) {
        return label;
      }
    }
  }
  return undefined;
}

export const nameserverSuffixes: Array<[suffix: string, label: string]> = [
  [".registrar-servers.com", "Namecheap"],
  [".domaincontrol.com", "GoDaddy"],
  [".ns.cloudflare.com", "Cloudflare"],
  [".awsdns-", "Route 53"],
  [".googledomains.com", "Google Domains"],
  [".dns.hostinger.com", "Hostinger"],
  [".porkbun.com", "Porkbun"],
  [".gandi.net", "Gandi"],
  [".dnsimple.com", "DNSimple"],
  [".squarespacedns.com", "Squarespace"],
  [".name-services.com", "Enom"],
  [".ovh.net", "OVH"],
  [".hetzner.com", "Hetzner"],
];
