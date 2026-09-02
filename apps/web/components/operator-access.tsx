"use client";

import { type FormEvent, useState, useSyncExternalStore } from "react";
import {
  KeyRound,
  LoaderCircle,
  ShieldCheck,
  Waypoints,
} from "lucide-react";

import { DNSConsole } from "@/components/dns-console";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  clearOperatorLockReason,
  getOperatorSessionSnapshot,
  getServerOperatorSessionSnapshot,
  setOperatorToken,
  subscribeToOperatorSession,
} from "@/lib/operator-session";

const rejectedTokenMessage =
  "That operator token was rejected or has expired. Enter a valid token to continue.";

export function OperatorAccess() {
  const session = useSyncExternalStore(
    subscribeToOperatorSession,
    getOperatorSessionSnapshot,
    getServerOperatorSessionSnapshot,
  );
  const [token, setToken] = useState("");
  const [storageError, setStorageError] = useState<string>();
  const error =
    storageError ??
    (session.status === "locked" && session.reason === "unauthenticated"
      ? rejectedTokenMessage
      : undefined);

  function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setStorageError(undefined);
    try {
      setOperatorToken(token);
      setToken("");
    } catch {
      setStorageError(
        "The token could not be saved in this tab. Allow session storage and try again.",
      );
    }
  }

  if (session.status === "unlocked") {
    return <DNSConsole />;
  }

  if (session.status === "checking") {
    return (
      <main
        className="flex min-h-screen items-center justify-center bg-background/94"
        aria-busy="true"
      >
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <LoaderCircle className="size-4 animate-spin" aria-hidden="true" />
          Checking operator access…
        </div>
      </main>
    );
  }

  return (
    <main className="relative flex min-h-screen items-center justify-center bg-background/94 px-4 py-16">
      <div className="absolute top-5 left-5 flex items-center gap-2.5 sm:top-7 sm:left-7">
        <div className="flex size-8 items-center justify-center rounded-md border border-primary/30 bg-primary/10 text-primary shadow-[0_0_24px_color-mix(in_oklch,var(--primary),transparent_78%)]">
          <Waypoints className="size-4" aria-hidden="true" />
        </div>
        <div className="leading-none">
          <p className="text-sm font-semibold tracking-tight">Simple DNS</p>
          <p className="mt-1 font-mono text-[0.6rem] tracking-[0.13em] text-muted-foreground uppercase">
            Operator console
          </p>
        </div>
      </div>

      <section
        className="w-full max-w-md border bg-card/92 p-6 shadow-2xl shadow-black/20 sm:p-8"
        aria-labelledby="operator-access-title"
      >
        <div className="flex size-11 items-center justify-center rounded-lg border border-primary/25 bg-primary/8 text-primary">
          <KeyRound className="size-5" aria-hidden="true" />
        </div>
        <h1
          id="operator-access-title"
          className="mt-5 text-xl font-semibold tracking-tight"
        >
          Operator access
        </h1>
        <p className="mt-2 text-sm leading-6 text-muted-foreground">
          Enter the token configured on the DNS control API to manage zones and
          records.
        </p>

        {error ? (
          <Alert variant="destructive" className="mt-5" id="operator-token-error">
            <KeyRound aria-hidden="true" />
            <AlertTitle>Access denied</AlertTitle>
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}

        <form className="mt-6 grid gap-4" onSubmit={handleSubmit}>
          <div className="grid gap-2">
            <Label htmlFor="operator-token">Operator token</Label>
            <Input
              id="operator-token"
              name="operator-token"
              type="password"
              autoComplete="off"
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              autoFocus
              required
              maxLength={4096}
              placeholder="Paste token"
              value={token}
              onChange={(event) => {
                setToken(event.target.value);
                setStorageError(undefined);
                clearOperatorLockReason();
              }}
              aria-invalid={error ? true : undefined}
              aria-describedby={
                error
                  ? "operator-token-help operator-token-error"
                  : "operator-token-help"
              }
            />
            <p
              id="operator-token-help"
              className="text-xs leading-5 text-muted-foreground"
            >
              Stored only in session storage for this browser tab. Closing the
              tab ends the session.
            </p>
          </div>
          <Button type="submit" size="lg" disabled={!token.trim()}>
            <KeyRound data-icon="inline-start" />
            Unlock console
          </Button>
        </form>

        <div className="mt-6 flex items-start gap-2 border-t pt-4 text-xs leading-5 text-muted-foreground">
          <ShieldCheck className="mt-0.5 size-3.5 shrink-0 text-primary/80" aria-hidden="true" />
          The token is attached only to Connect RPC requests and is never stored
          in cookies or local storage.
        </div>
      </section>
    </main>
  );
}
