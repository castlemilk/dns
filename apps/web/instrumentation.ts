import type { Instrumentation } from "next";

/**
 * Server-only telemetry boundary.
 *
 * Browser telemetry is intentionally not registered here: the operator token
 * lives in sessionStorage and managed DNS data is rendered in the browser. A
 * future client telemetry pass must prove that neither can enter attributes,
 * events, URLs, or baggage before it is enabled.
 */
export async function register() {
  if (process.env.NEXT_RUNTIME === "nodejs") {
    await import("./instrumentation.node");
  }
}

export const onRequestError: Instrumentation.onRequestError = async (
  _error,
  request,
  context,
) => {
  if (process.env.NEXT_RUNTIME !== "nodejs") {
    return;
  }

  const { recordServerError } = await import("./observability/metrics");
  // Deliberately use Next's route template and method only. request.path,
  // headers, the error object, and its digest/message can contain secrets or
  // user-controlled DNS data and must never cross the telemetry boundary.
  recordServerError({
    method: request.method,
    route: context.routePath,
    routeType: context.routeType,
  });
};
