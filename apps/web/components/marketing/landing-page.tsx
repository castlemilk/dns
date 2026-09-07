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
 * deployment that runs no hosting engine never advertises deploys, never illustrates a
 * mailbox, and a price is only ever the one the billing engine reported.
 */

export type LandingPageProps = {
  plan?: PublicPlan;
};

type Product = {
  id: string;
  eyebrow: string;
  title: string;
  body: string;
  points: string[];
};

const dnsOnlyWebsite: Product = {
  id: "website",
  eyebrow: "WEBSITE",
  title: "Point it anywhere",
  body: "Enter the IP or host your site lives on and the records are written for you — Vercel, Fly, Netlify or your own server.",
  points: ["A, AAAA and www together", "Apex and subdomain handled", "Change it later without touching the zone"],
};

const dnsOnlyEmail: Product = {
  id: "email",
  eyebrow: "EMAIL",
  title: "Mail routing, done right",
  body: "Pick your provider and the records that make mail deliverable are generated as one set, not one at a time.",
  points: ["MX, SPF and DMARC together", "DKIM when your provider gives you the key", "No half-configured domains"],
};

const dnsProduct: Product = {
  id: "dns",
  eyebrow: "DNS",
  title: "Authoritative, and yours",
  body: "Every guided step writes the records it needs. Add your own for anything else, edit the raw zone directly, or import and export it.",
  points: ["Self-hosted authoritative nameservers", "The raw zone one click away", "Import and export a zone file"],
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

  const websiteProduct: Product = hosting
    ? {
        id: "website",
        eyebrow: "HOSTING",
        title: "Deploy from GitHub",
        body: `Connect a repository${
          uploads ? " or drop a folder" : ""
        }. Static sites, Node and Next.js build on our hosting engine and go live over HTTPS.`,
        points: [
          "Static, Node and Next.js builds",
          "HTTPS provisioned for you",
          uploads ? "Push to deploy, or drop a folder" : "Push to deploy",
        ],
      }
    : dnsOnlyWebsite;

  const emailProduct: Product = mail
    ? {
        id: "email",
        eyebrow: "EMAIL",
        title: "Mailboxes that just work",
        body: "Add an address and it is ready to use. The records that make mail deliverable are published for you.",
        points: [
          "Real mailboxes on your own domain",
          "SPF, DKIM and DMARC published automatically",
          "Forwarding without a second provider",
        ],
      }
    : dnsOnlyEmail;

  const products = [websiteProduct, emailProduct, dnsProduct];

  // Only the engines this deployment runs are ever named as included.
  const included = [
    hosting ? "hosting" : undefined,
    mail && mailboxes > 0
      ? `${mailboxes} ${plural(mailboxes, "mailbox", "mailboxes")}`
      : undefined,
    "DNS",
  ].filter((part): part is string => part !== undefined);

  const headline =
    hosting || mail
      ? "Your website, email and DNS. One place, no zone files."
      : "Your domain, website and email. One place, no zone files.";

  const subtitle =
    hosting || mail
      ? `Connect a domain${hosting ? ", deploy a repo" : ""}${
          mail ? ", add a mailbox" : ""
        }. The DNS writes itself. When you want the raw records, they're right there.`
      : undefined;

  // The last step is only ever the work this deployment can actually do.
  const finalStep = hosting
    ? {
        title: "Deploy",
        body: mail
          ? "Connect a repository and it builds and goes live. Add a mailbox and it is ready to use."
          : "Connect a repository and it builds and goes live over HTTPS.",
      }
    : {
        title: mail ? "Point and send" : "Point it where it lives",
        body: mail
          ? "Tell us where your site lives and add a mailbox. Every record either needs is written for you."
          : "Tell us where your site lives and who handles your mail. Every record either needs is written for you.",
      };

  const steps = [
    {
      title: "Add your domain",
      body: "Enter the domain you own. Its zone is created, with the records a working domain needs already in place.",
    },
    {
      title: "Point the nameservers",
      body: "Paste two nameservers at your registrar. We show you exactly what to enter, and tell you when it has taken effect.",
    },
    finalStep,
  ];

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
              {headline}
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
            {products.map((product) => (
              <div
                key={product.id}
                id={product.id}
                className="flex scroll-mt-8 flex-col gap-2.5"
              >
                <p className="font-mono text-xs font-medium text-primary">
                  {product.eyebrow}
                </p>
                <h2 className="text-xl font-semibold">{product.title}</h2>
                <p className="text-[14.5px] leading-[1.6] text-subtle">
                  {product.body}
                </p>
                <ul className="mt-1.5 flex flex-col gap-1.5">
                  {product.points.map((point) => (
                    <li
                      key={point}
                      className="flex gap-2.5 text-[14px] leading-[1.5] text-subtle"
                    >
                      <span
                        aria-hidden="true"
                        className="mt-[7px] size-1 shrink-0 rounded-full bg-primary"
                      />
                      {point}
                    </li>
                  ))}
                </ul>
              </div>
            ))}
          </section>

          <section
            id="how"
            className="scroll-mt-8 border-t border-line px-6 py-16 lg:px-16 lg:py-24"
          >
            <h2 className="text-[26px] font-semibold tracking-[-0.01em] sm:text-[30px]">
              Three steps to a working domain
            </h2>
            <ol className="mt-10 grid gap-10 md:grid-cols-3">
              {steps.map((step, index) => (
                <li key={step.title} className="flex flex-col gap-2.5">
                  <span className="font-mono text-xs font-medium text-primary">
                    {`0${index + 1}`}
                  </span>
                  <h3 className="text-lg font-semibold">{step.title}</h3>
                  <p className="text-[14.5px] leading-[1.6] text-subtle">
                    {step.body}
                  </p>
                </li>
              ))}
            </ol>
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
