import type { Attributes } from "@opentelemetry/api";

// This explicit inventory is the cardinality boundary. Add new route templates
// here as server routes are introduced; raw request paths are never accepted.
const knownRoutes = new Set([
  "/",
  "/_not-found",
  "/api/health",
  "/api/nameservers",
  "/connect",
  "/domains",
  "/domains/[zone]",
  "/icon.svg",
]);
const knownMethods = new Set([
  "CONNECT",
  "DELETE",
  "GET",
  "HEAD",
  "OPTIONS",
  "PATCH",
  "POST",
  "PUT",
  "TRACE",
]);
const knownRouteTypes = new Set(["action", "proxy", "render", "route"]);

export const normalizeRoute = (value: unknown) =>
  typeof value === "string" && knownRoutes.has(value) ? value : "other";

export const normalizeMethod = (value: unknown) => {
  const method = typeof value === "string" ? value.trim().toUpperCase() : "";
  return knownMethods.has(method) ? method : "OTHER";
};

export const normalizeRouteType = (value: unknown) =>
  typeof value === "string" && knownRouteTypes.has(value) ? value : "other";

export const normalizeStatusCode = (value: unknown) => {
  const numeric = typeof value === "number" ? value : Number(value);
  return Number.isInteger(numeric) && numeric >= 100 && numeric <= 599
    ? numeric
    : 0;
};

export const statusClass = (statusCode: number) =>
  statusCode === 0 ? "unknown" : `${Math.floor(statusCode / 100)}xx`;

export const requestAttributes = ({
  method,
  route,
  statusCode,
}: {
  method: unknown;
  route: unknown;
  statusCode: unknown;
}): Attributes => {
  const normalizedStatus = normalizeStatusCode(statusCode);
  return {
    "http.request.method": normalizeMethod(method),
    "http.response.status_class": statusClass(normalizedStatus),
    "http.route": normalizeRoute(route),
  };
};
