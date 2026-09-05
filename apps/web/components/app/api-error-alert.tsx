"use client";

import { TriangleAlert } from "lucide-react";

import { useZones } from "@/components/app/zones-provider";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";

export function ApiErrorAlert() {
  const { error, loadedAt, refreshing, refresh, clearError } = useZones();

  if (!error) {
    return null;
  }

  return (
    <Alert variant="destructive">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>Control API request failed</AlertTitle>
      <AlertDescription>
        {/* Only true once a list has actually arrived; before that there is no data
            to keep showing, and the page says so itself. */}
        <div>
          {error}
          {loadedAt !== undefined ? " Existing data remains visible." : ""}
        </div>
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <Button
            variant="outline"
            size="sm"
            disabled={refreshing}
            onClick={() => {
              void refresh();
            }}
          >
            Retry
          </Button>
          <Button variant="ghost" size="sm" onClick={clearError}>
            Dismiss
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  );
}
