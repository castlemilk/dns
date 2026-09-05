import { DnsCard } from "@/components/domain/dns-card";
import { DomainHeader } from "@/components/domain/domain-header";
import { EmailCard } from "@/components/domain/email-card";
import { WebsiteCard } from "@/components/domain/website-card";
import type { PublicPlan } from "@/lib/public-plan";
import {
  sampleBillingLine,
  sampleDeployedLabel,
  sampleForwarders,
  sampleMailDomain,
  sampleMailboxes,
  sampleSite,
} from "@/lib/sample-platform";
import { sampleUpdatedLabel, sampleZone } from "@/lib/sample-zone";
import { deriveDomain } from "@/lib/zone-model";

/**
 * The product screenshot on the landing page: the real domain-view components rendered
 * on the server from a hard-coded sample domain, clipped to a fixed 520 px frame like
 * the design's screenshot slot.
 *
 * This is the only module allowed to import `@/lib/sample-zone` and
 * `@/lib/sample-platform` (enforced in `eslint.config.mjs`), and the only place in the
 * app where data that did not come from the control API is rendered — hence the
 * "Example · sample domain" chip.
 *
 * The sample site, mail domain and subscription line are shown only for the engines this
 * deployment actually runs: a DNS-only deployment's landing page must not illustrate a
 * capability it does not have.
 *
 * Everything under the frame is static: dates arrive as label strings and the card
 * footers as `footer` nodes, and no callbacks are passed, so no copy button, no sheet
 * and no clock-reading output reaches the page and the prerendered HTML never hydrates
 * to different text. (The §12.14 audit greps this directory for the relative-time
 * component's name, so it is not spelled out here.)
 */

export type ProductFrameProps = {
  plan?: PublicPlan;
};

const actionClassName =
  "text-ui inline-flex h-9 items-center rounded-[7px] border border-input px-3.5 text-light";

const staticLink = "text-link";

export function ProductFrame({ plan }: ProductFrameProps) {
  const model = deriveDomain(sampleZone);

  const hosting = plan?.hostingConfigured ?? false;
  const mail = plan?.mailConfigured ?? false;
  const billing = (plan?.billingConfigured ?? false) && plan?.priceLabel !== "";

  const site = hosting ? sampleSite : undefined;
  const mailDomain = mail ? sampleMailDomain : undefined;

  return (
    // A <figure> rather than a <div>: the description has to sit on an element that
    // takes an accessible name and stays outside the `inert` subtree, which is pruned
    // from the accessibility tree along with any aria-label on it.
    <figure
      id="product"
      aria-label="Preview of the domain view for a sample domain"
      className="relative mx-6 h-[520px] overflow-hidden rounded-[14px] border border-line-strong bg-background lg:mx-16"
    >
      <div
        aria-hidden="true"
        className="absolute inset-0 bg-[radial-gradient(600px_240px_at_50%_0%,rgba(91,156,255,.14),transparent)]"
      />

      <div inert className="relative flex flex-col gap-7 p-6 lg:p-10">
        <DomainHeader
          zoneName={model.zone.name}
          healthTone={model.tone}
          healthLabel={model.healthLabel}
          serial={model.serial}
          updatedLabel={sampleUpdatedLabel}
          nameservers={model.dns.nameservers}
          billingLine={billing ? sampleBillingLine : undefined}
          actions={
            <>
              <span className={actionClassName}>Visit site ↗</span>
              <span className={actionClassName}>Settings</span>
            </>
          }
        />

        <div className="grid gap-4 md:grid-cols-3">
          <WebsiteCard
            zoneName={model.zone.name}
            website={model.website}
            updatedLabel={sampleUpdatedLabel}
            site={site}
            hostingConfigured={hosting}
            deployedLabel={sampleDeployedLabel}
            footer={
              hosting ? (
                <>
                  <span className={staticLink}>Deploy</span>
                  <span className={staticLink}>Deploy log</span>
                  <span className={staticLink}>Rollback</span>
                </>
              ) : (
                <>
                  <span className={staticLink}>Change where it points</span>
                  <span className={staticLink}>Show records</span>
                </>
              )
            }
          />
          <EmailCard
            zoneName={model.zone.name}
            email={model.email}
            updatedLabel={sampleUpdatedLabel}
            mailDomain={mailDomain}
            mailboxes={mail ? sampleMailboxes : undefined}
            forwarders={mail ? sampleForwarders : undefined}
            mailConfigured={mail}
            footer={
              mail ? (
                <>
                  <span className={staticLink}>Add mailbox</span>
                  <span className={staticLink}>Add forwarder</span>
                  <span className={staticLink}>Mail queue</span>
                </>
              ) : (
                <>
                  <span className={staticLink}>Change routing</span>
                  <span className={staticLink}>Add DKIM key</span>
                  <span className={staticLink}>Show records</span>
                </>
              )
            }
          />
          <DnsCard
            zoneName={model.zone.name}
            groups={model.groups}
            total={model.dns.total}
            rawShown={false}
            engineManaged={Boolean(site) || Boolean(mailDomain)}
            footer={
              <>
                <span className={staticLink}>Add record</span>
                <span className={staticLink}>Verify a service</span>
                <span className="flex-1" />
                <span className={staticLink}>Show raw records</span>
              </>
            }
          />
        </div>
      </div>

      <div
        aria-hidden="true"
        className="absolute inset-x-0 bottom-0 h-16 bg-gradient-to-t from-page"
      />
      <figcaption className="eyebrow absolute top-3 right-4">
        Example · sample domain
      </figcaption>
    </figure>
  );
}
