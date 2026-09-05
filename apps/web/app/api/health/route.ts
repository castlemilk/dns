import { recordHealthRequest } from "@/observability/metrics";

export function GET() {
  const startedAt = performance.now();
  let statusCode = 500;
  try {
    const response = Response.json({ status: "ok" });
    statusCode = response.status;
    return response;
  } finally {
    recordHealthRequest((performance.now() - startedAt) / 1_000, statusCode);
  }
}
