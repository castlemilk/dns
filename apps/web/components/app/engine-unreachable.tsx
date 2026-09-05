import { TriangleAlert } from "lucide-react";

import { RelativeTime } from "@/components/app/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import type { EngineName } from "@/lib/platform-model";

export type EngineUnreachableProps = {
  engine: EngineName;
  /** The bounded, redacted probe reason from the control plane. */
  reason: string;
  checkedAt?: Date;
  onRetry: () => void;
};

const engineWord: Record<EngineName, string> = {
  hosting: "hosting",
  mail: "mail",
  billing: "billing",
};

/**
 * Shown inside the card or page whose data is missing, never as the global API alert:
 * an engine that did not answer must not make the zone list look broken.
 */
export function EngineUnreachable({
  engine,
  reason,
  checkedAt,
  onRetry,
}: EngineUnreachableProps) {
  return (
    <Alert>
      <TriangleAlert aria-hidden="true" className="text-warning" />
      <AlertTitle>The {engineWord[engine]} engine didn&apos;t answer</AlertTitle>
      <AlertDescription>
        <div className="text-soft">{reason || "No reason was reported."}</div>
        {checkedAt ? (
          <div className="text-xs text-muted-foreground">
            Checked <RelativeTime date={checkedAt} />
          </div>
        ) : null}
        <div className="mt-2">
          <Button variant="outline" size="sm" onClick={onRetry}>
            Retry
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  );
}
