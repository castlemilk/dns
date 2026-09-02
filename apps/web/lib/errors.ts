import { ConnectError } from "@connectrpc/connect";

export function describeError(error: unknown): string {
  const message = ConnectError.from(error).rawMessage.trim();
  return message || "The request could not be completed.";
}
