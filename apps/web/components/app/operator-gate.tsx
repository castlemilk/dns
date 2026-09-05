"use client";

import Link from "next/link";
import { useState, useSyncExternalStore, type FormEvent } from "react";
import { KeyRound, ShieldCheck } from "lucide-react";

import { BrandMark } from "@/components/app/brand-mark";
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

export function OperatorGate() {
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

  return (
    <main className="relative flex min-h-dvh items-center justify-center bg-background px-4 py-16">
      <BrandMark href="/" className="absolute top-6 left-6" />

      <section
        className="w-full max-w-[420px] rounded-xl border border-line bg-card p-7"
        aria-labelledby="operator-access-title"
      >
        <div className="flex size-10 items-center justify-center rounded-lg bg-primary/10 text-primary">
          <KeyRound className="size-[18px]" aria-hidden="true" />
        </div>
        <h1
          id="operator-access-title"
          className="mt-5 text-xl leading-tight font-semibold"
        >
          Operator access
        </h1>
        <p className="text-ui mt-2 leading-[1.55] text-subtle">
          Enter the token configured on the DNS control API to manage domains
          and records.
        </p>

        {error ? (
          <Alert
            variant="destructive"
            className="mt-5"
            id="operator-token-error"
          >
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

        <div className="mt-6 flex items-start gap-2 border-t border-line-soft pt-4 text-xs leading-5 text-muted-foreground">
          <ShieldCheck
            className="mt-0.5 size-3.5 shrink-0 text-primary/80"
            aria-hidden="true"
          />
          The token is attached only to Connect RPC requests and is never stored
          in cookies or local storage.
        </div>

        <div className="mt-5 text-[13px]">
          <Link href="/" className="link">
            ← Back to simple
          </Link>
        </div>
      </section>
    </main>
  );
}
