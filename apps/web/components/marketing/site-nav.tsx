import Link from "next/link";

import { BrandMark } from "@/components/app/brand-mark";
import { Button } from "@/components/ui/button";

export type SiteNavProps = {
  /** The hosting engine is configured, so the first section is about hosting. */
  hosting?: boolean;
  /** Billing is configured and reported a price, so there is a pricing section. */
  pricing?: boolean;
};

/**
 * Landing-page navigation. The centre links point at the sections this deployment
 * actually has: the design's Docs link has nothing behind it, and the Pricing link only
 * appears when the control plane reported a price. The primary action is the real
 * "Connect a domain" route — this service has no signup.
 */
export function SiteNav({ hosting = false, pricing = false }: SiteNavProps) {
  const sections: Array<{ href: string; label: string }> = [
    { href: "#website", label: hosting ? "Hosting" : "Website" },
    { href: "#email", label: "Email" },
    { href: "#dns", label: "DNS" },
  ];
  if (pricing) {
    sections.push({ href: "#pricing", label: "Pricing" });
  }

  return (
    <header className="flex items-center justify-between gap-4 px-6 py-5 lg:px-16 lg:py-[22px]">
      <BrandMark href="/" />

      <nav
        aria-label="Sections"
        className="hidden gap-7 text-sm text-subtle md:flex"
      >
        {sections.map((section) => (
          <a
            key={section.href}
            href={section.href}
            className="rounded-sm outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
          >
            {section.label}
          </a>
        ))}
      </nav>

      <div className="flex items-center gap-2.5">
        <Link
          href="/domains"
          className="rounded-sm px-2.5 text-sm text-subtle outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
        >
          Log in
        </Link>
        <Button
          size="lg"
          className="text-ui rounded-[7px] px-3.5 font-semibold"
          asChild
        >
          <Link href="/connect">Connect a domain</Link>
        </Button>
      </div>
    </header>
  );
}
