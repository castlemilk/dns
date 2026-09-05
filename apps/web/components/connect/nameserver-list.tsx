import { CopyButton } from "@/components/app/copy-button";
import { stripDot } from "@/lib/dns-values";
import { cn } from "@/lib/utils";

export type NameserverListProps = {
  nameservers: string[];
  size?: "md" | "lg";
  className?: string;
};

/**
 * The nameservers this service answers the zone from, exactly as `zone.nameservers`
 * reports them (trailing dot stripped for copying into a registrar form).
 */
export function NameserverList({
  nameservers,
  size = "md",
  className,
}: NameserverListProps) {
  const hosts = nameservers.map((host) => stripDot(host)).filter(Boolean);

  return (
    <div
      data-slot="nameserver-list"
      className={cn(
        "overflow-hidden rounded-xl border border-line-strong bg-card",
        className,
      )}
    >
      {hosts.length === 0 ? (
        <p className="text-ui px-[18px] py-3.5 text-muted-foreground">
          The control API did not report nameservers for this zone.
        </p>
      ) : (
        <ul>
          {hosts.map((host) => (
            <li
              key={host}
              className="flex items-center justify-between gap-4 border-b border-line-soft px-[18px] py-3.5 last:border-b-0"
            >
              <span
                className={cn(
                  "font-mono break-all",
                  size === "lg" ? "text-base" : "text-[15px]",
                )}
              >
                {host}
              </span>
              <CopyButton value={host} className="font-sans text-[13px]" />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
