"use client";

import Link from "next/link";
import { useEffect, useReducer } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { Info } from "lucide-react";

import { usePlatform } from "@/components/app/platform-provider";
import { Alert, AlertTitle } from "@/components/ui/alert";
import type { ConfirmCheckoutResponse } from "@/gen/billing/v1/billing_pb";
import { describeError } from "@/lib/errors";

export type CheckoutSuccessProps = {
  /** `?session_id=` from the server component; confirmed against this control plane. */
  sessionId?: string;
  /** `?return=` from the server component; validated before it is used as a link. */
  returnPath?: string;
};

/**
 * Where Stripe (or the fake provider) sends the browser after a checkout. The page
 * confirms the session against this control plane rather than trusting the redirect:
 * `ConfirmCheckout` only answers for sessions this control plane created, and the state
 * shown is the one the facade recorded, not the one the URL claims.
 *
 * Nothing here says "subscribed" until `completed` is true. While the webhook is still
 * in flight it says it is waiting, and after 30 s it says the Billing page will update
 * when the webhook arrives — which is what actually happens.
 */

/** `/billing` or a domain page, and nothing else — the value comes from the query string. */
const returnPattern = /^\/(billing|domains\/[a-z0-9.-]+)$/;

function safeReturnPath(raw?: string): string {
  return raw && returnPattern.test(raw) ? raw : "/billing";
}

const pollIntervalMs = 2_000;
const pollBudgetMs = 30_000;

type State = {
  phase: "confirming" | "waiting" | "done" | "timeout" | "unknown" | "error";
  response?: ConfirmCheckoutResponse;
  error?: string;
};

type Action =
  | { type: "waiting"; response: ConfirmCheckoutResponse }
  | { type: "done"; response: ConfirmCheckoutResponse }
  | { type: "timeout" }
  | { type: "unknown" }
  | { type: "error"; message: string };

function reducer(state: State, action: Action): State {
  switch (action.type) {
    case "waiting":
      return { phase: "waiting", response: action.response };
    case "done":
      return { phase: "done", response: action.response };
    case "timeout":
      return { ...state, phase: "timeout" };
    case "unknown":
      return { phase: "unknown" };
    case "error":
      return { phase: "error", error: action.message };
    default:
      return state;
  }
}

export function CheckoutSuccess({ sessionId, returnPath }: CheckoutSuccessProps) {
  const { confirmCheckout } = usePlatform();
  const [state, dispatch] = useReducer(reducer, { phase: "confirming" });
  const back = safeReturnPath(returnPath);

  useEffect(() => {
    if (!sessionId) {
      return;
    }
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const deadline = Date.now() + pollBudgetMs;

    async function run() {
      try {
        const response = await confirmCheckout(sessionId ?? "");
        if (cancelled) {
          return;
        }
        if (response.completed) {
          dispatch({ type: "done", response });
          return;
        }
        dispatch({ type: "waiting", response });
        if (Date.now() < deadline) {
          timer = setTimeout(() => {
            void run();
          }, pollIntervalMs);
        } else {
          dispatch({ type: "timeout" });
        }
      } catch (error) {
        if (cancelled) {
          return;
        }
        if (ConnectError.from(error).code === Code.NotFound) {
          dispatch({ type: "unknown" });
          return;
        }
        dispatch({ type: "error", message: describeError(error) });
      }
    }

    void run();
    return () => {
      cancelled = true;
      if (timer !== undefined) {
        clearTimeout(timer);
      }
    };
  }, [confirmCheckout, sessionId]);

  const fake = state.response?.provider === "fake";
  const zoneName = state.response?.domain?.zoneName;

  return (
    <section
      data-slot="checkout-success"
      className="mx-auto flex w-full max-w-[560px] flex-col gap-5 rounded-[10px] border border-line px-6 py-10 text-center"
    >
      <h1 className="text-xl font-semibold">Billing</h1>

      {fake ? (
        <Alert role="status">
          <Info aria-hidden="true" className="text-primary" />
          <AlertTitle>Fake billing provider — no real charges.</AlertTitle>
        </Alert>
      ) : null}

      <p
        className="text-ui mx-auto max-w-[46ch] text-muted-foreground"
        aria-live="polite"
        role={state.phase === "error" ? "alert" : undefined}
      >
        {!sessionId
          ? "No checkout session was passed."
          : state.phase === "unknown"
            ? "This checkout session isn't known to this control plane."
            : state.phase === "error"
              ? state.error
              : state.phase === "done"
                ? `${fake ? "Fake checkout · " : ""}Checkout complete — ${
                    zoneName ?? "the domain"
                  } is subscribed.${
                    state.response?.paymentRequired === false
                      ? " No payment was required."
                      : ""
                  }`
                : state.phase === "timeout"
                  ? "Stripe hasn't confirmed yet — the Billing page updates when the webhook arrives."
                  : state.phase === "waiting"
                    ? "Waiting for Stripe to confirm…"
                    : "Confirming the checkout…"}
      </p>

      <p className="text-[13px]">
        <Link href={back} className="link">
          {back === "/billing" ? "Back to billing →" : "Back to the domain →"}
        </Link>
      </p>
    </section>
  );
}
