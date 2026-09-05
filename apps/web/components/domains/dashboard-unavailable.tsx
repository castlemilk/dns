import { Button } from "@/components/ui/button";

export type DashboardUnavailableProps = {
  /** The last load failure from the provider, if it has not been dismissed. */
  error?: string;
  /** True while a retry is in flight. */
  retrying?: boolean;
  onRetry: () => void;
};

/**
 * Shown when the first listZones never succeeded: the zone list is unknown, so the
 * dashboard must not claim "No domains yet" or summarise zero domains. Distinct from
 * the empty state, which means the API really did report an empty list.
 */
export function DashboardUnavailable({
  error,
  retrying,
  onRetry,
}: DashboardUnavailableProps) {
  return (
    <div className="rounded-[10px] border border-line px-6 py-14 text-center">
      <h2 className="text-xl font-semibold">Domains couldn’t be loaded</h2>
      <p className="text-ui mx-auto mt-2.5 max-w-md text-subtle">
        The control API has not answered yet, so this list is unknown — it is not
        a sign that you have no domains.
      </p>
      {error ? (
        <p className="mx-auto mt-2 max-w-md text-[13px] text-muted-foreground">
          {error}
        </p>
      ) : null}
      <div className="mt-6 flex justify-center">
        <Button
          size="lg"
          variant="outline"
          className="text-ui rounded-[7px] px-3.5"
          onClick={onRetry}
          disabled={retrying}
        >
          {retrying ? "Retrying…" : "Retry"}
        </Button>
      </div>
    </div>
  );
}
