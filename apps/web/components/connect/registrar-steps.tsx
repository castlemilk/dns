import { cn } from "@/lib/utils";

export type RegistrarStepsProps = {
  zoneName: string;
  /** Opens the zone-file import dialog; the hint is omitted when absent. */
  onImport?: () => void;
  className?: string;
};

/**
 * What to do at the registrar. Deliberately provider-neutral: NS records identify the
 * DNS host a domain currently uses, never the registrar where nameservers are changed,
 * so no company is named and no "open your registrar" link is offered.
 */
export function RegistrarSteps({
  zoneName,
  onImport,
  className,
}: RegistrarStepsProps) {
  const labels = zoneName.split(".");
  const isSubdomain = labels.length >= 3;

  const steps = [
    <>
      Log in to your registrar — the company you bought {zoneName} from — and
      open its nameserver (DNS) settings. Choose custom nameservers.
    </>,
    <>Paste the nameservers above and save.</>,
    <>
      Come back here. This page re-checks every 30 seconds while it&rsquo;s open;
      registrars and caches can take from minutes to a day.
      {onImport ? (
        <>
          {" "}
          Have a zone file from the old provider?{" "}
          <button
            type="button"
            onClick={onImport}
            className="link rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            Import it
          </button>
        </>
      ) : null}
    </>,
  ];

  if (isSubdomain) {
    steps.push(
      <>
        If {zoneName} is a subdomain of a domain you host elsewhere, add NS
        records for &ldquo;{labels[0]}&rdquo; pointing at the nameservers above
        in the parent zone instead.
      </>,
    );
  }

  return (
    <ol
      data-slot="registrar-steps"
      className={cn("flex flex-col gap-2.5", className)}
    >
      {steps.map((content, index) => (
        <li
          key={index}
          className="text-ui flex gap-2.5 leading-[1.55] text-subtle"
        >
          <span aria-hidden="true" className="font-mono text-faint">
            {index + 1}
          </span>
          <span>{content}</span>
        </li>
      ))}
    </ol>
  );
}
