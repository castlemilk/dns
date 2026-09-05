import Link from "next/link";

import { ProductFrame } from "@/components/marketing/product-frame";
import { SiteFooter } from "@/components/marketing/site-footer";
import { SiteNav } from "@/components/marketing/site-nav";
import { Button } from "@/components/ui/button";
import { plural } from "@/lib/format";
import type { PublicPlan } from "@/lib/public-plan";

/**
 * The public landing page at `/`. Entirely static server components: no hooks, no
 * client-only text, nothing that ticks.
 *
 * What it claims is decided by `GET /public/v1/plan` — the same control plane the
 * console talks to. With no plan (the variable is unset, or the control API did not
 * answer) or with every engine unconfigured it is the DNS-only page, word for word: a
 * deployment that runs no hosting engine never advertises deploys, and a price is only
 * ever the one the billing engine reported.
 */

export type LandingPageProps = {
  plan?: PublicPlan;
};

type Feature = { id: string; eyebrow: string; title: string; body: string };

const dnsOnlyWebsite: Feature = {
  id: "website",
  eyebrow: "WEBSITE",
  title: "Point it anywhere",
  body: "Enter the IP or host your site lives on. The A, AAAA and www records are written for you — Vercel, Fly, Netlify or your own server.",
};

const dnsOnlyEmail: Feature = {
  id: "email",
  eyebrow: "EMAIL",
  title: "Mail routing, done right",
  body: "Pick your provider and the MX, SPF and DMARC records are generated together. Add DKIM when your provider gives you the key.",
};

const dnsFeature: Feature = {
  id: "dns",
  eyebrow: "DNS",
  title: "Records, written for you",
  body: "Every guided step creates the records it needs. Add your own for anything else, or edit the raw zone directly, or import and export it.",
};

const dnsOnlyCapabilities =
  "Self-hosted authoritative DNS · guided website and email records · raw zone one click away";

/** "a", "a and b", "a, b and c" — the pricing line reads as a sentence. */
function listSentence(parts: string[]): string {
  if (parts.length < 2) {
    return parts[0] ?? "";
  }
  return `${parts.slice(0, -1).join(", ")} and ${parts[parts.length - 1]}`;
}

export function LandingPage({ plan }: LandingPageProps) {
  const hosting = plan?.hostingConfigured ?? false;
  const uploads = plan?.uploadsEnabled ?? false;
  const mail = plan?.mailConfigured ?? false;
  const mailboxes = plan?.mailboxesPerDomain ?? 0;
  const priceLabel = plan?.priceLabel ?? "";
  const pricing = (plan?.billingConfigured ?? false) && priceLabel !== "";

  const websiteFeature: Feature = hosting
    ? {
        id: "website",
        eyebrow: "WEBSITE",
        title: "Deploy from GitHub",
        body: `Connect a repository${
          uploads ? " or drop a folder" : ""
        }. Static sites, Node and Next.js build on our hosting engine and go live over HTTPS.`,
      }
    : dnsOnlyWebsite;

  const emailFeature: Feature = mail
    ? {
        id: "email",
        eyebrow: "EMAIL",
        title: "Mailboxes that just work",
        body: "Add an address and it's ready to use. SPF, DKIM and DMARC are published for you.",
      }
    : dnsOnlyEmail;

  const features = [websiteFeature, emailFeature, dnsFeature];

  // Only the engines this deployment runs are ever named as included.
  const included = [
    hosting ? "hosting" : undefined,
    mail && mailboxes > 0
      ? `${mailboxes} ${plural(mailboxes, "mailbox", "mailboxes")}`
      : undefined,
    "DNS",
  ].filter((part): part is string => part !== undefined);

  const subtitle =
    hosting || mail
      ? `Connect a domain${hosting ? ", deploy a repo" : ""}${
          mail ? ", add a mailbox" : ""
        }. The DNS writes itself. When you want the raw records, they're right there.`
      : undefined;

  return (
    <div className="min-h-dvh bg-page text-foreground">
      <div className="mx-auto max-w-[1440px]">
        <SiteNav hosting={hosting} pricing={pricing} />

        {/* One landmark around everything between the banner and the footer, so
            landmark navigation reaches the page's content like it does in the app
            shell, the gate and the 404. */}
        <main>
          <section className="flex flex-col items-center gap-6 px-6 pt-16 pb-12 text-center lg:px-16 lg:pt-24 lg:pb-[72px]">
            <h1 className="max-w-[820px] text-[40px] leading-[1.05] font-semibold tracking-[-0.02em] text-balance sm:text-[52px] lg:text-[60px]">
              Your domain, website and email. One place, no zone files.
            </h1>
            <p className="max-w-[600px] text-lg leading-[1.55] text-pretty text-subtle">
              {subtitle ?? (
                <>
                  Point your website, route your email — from one DNS console.
                  Connect a domain and the records write themselves; when you want
                  the raw zone, it&rsquo;s right there.
                </>
              )}
            </p>
            <div className="mt-2 flex flex-wrap justify-center gap-3">
              <Button
                className="h-auto rounded-lg px-[22px] py-[13px] text-[15px] font-semibold"
                asChild
              >
                <Link href="/connect">Connect a domain</Link>
              </Button>
              <Button
                variant="outline"
                className="h-auto rounded-lg border-line-strong px-[22px] py-[13px] text-[15px] font-semibold"
                asChild
              >
                <Link href="/domains">Open the console</Link>
              </Button>
            </div>
            <p className="mt-2 font-mono text-[13px] text-muted-foreground">
              {pricing
                ? `${priceLabel} · ${listSentence(included)} included`
                : dnsOnlyCapabilities}
            </p>
          </section>

          <ProductFrame plan={plan} />

          <section className="grid gap-12 px-6 py-16 lg:px-16 lg:py-24 md:grid-cols-3">
            {features.map((feature) => (
              <div
                key={feature.id}
                id={feature.id}
                className="flex scroll-mt-8 flex-col gap-2.5"
              >
                <p className="font-mono text-xs font-medium text-primary">
                  {feature.eyebrow}
                </p>
                <h2 className="text-xl font-semibold">{feature.title}</h2>
                <p className="text-[14.5px] leading-[1.6] text-subtle">
                  {feature.body}
                </p>
              </div>
            ))}
          </section>

          {pricing ? (
            <section
              id="pricing"
              className="scroll-mt-8 px-6 pb-16 lg:px-16 lg:pb-24"
            >
              <div className="mx-auto flex max-w-[520px] flex-col gap-4 rounded-[14px] border border-line-strong px-6 py-8 text-center">
                <p className="font-mono text-xs font-medium text-primary">
                  PRICING
                </p>
                <p className="font-mono text-[15px] font-medium">{priceLabel}</p>
                <ul className="text-ui flex flex-col gap-1.5 text-subtle">
                  {hosting ? <li>Hosting for one site per domain</li> : null}
                  {mail && mailboxes > 0 ? (
                    <li>
                      {mailboxes} {plural(mailboxes, "mailbox", "mailboxes")} per
                      domain
                    </li>
                  ) : null}
                  <li>Authoritative DNS with the raw zone one click away</li>
                </ul>
                {plan?.policyNote ? (
                  <p className="text-[13px] text-muted-foreground">
                    {plan.policyNote}
                  </p>
                ) : null}
              </div>
            </section>
          ) : null}
        </main>

        <SiteFooter />
      </div>
    </div>
  );
}
