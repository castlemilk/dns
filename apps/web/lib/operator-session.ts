"use client";

export type OperatorLockReason = "manual" | "unauthenticated";
export type OperatorSessionSnapshot =
  | { status: "checking" }
  | { status: "locked"; reason?: OperatorLockReason }
  | { status: "unlocked" };

const operatorTokenKey = "deephost.operator-token.v1";
const checkingSnapshot: OperatorSessionSnapshot = { status: "checking" };
const lockedSnapshot: OperatorSessionSnapshot = { status: "locked" };
const unlockedSnapshot: OperatorSessionSnapshot = { status: "unlocked" };
const sessionListeners = new Set<() => void>();
let clientSnapshot: OperatorSessionSnapshot | undefined;

function notifySessionListeners(): void {
  for (const listener of sessionListeners) {
    listener();
  }
}

export function getOperatorToken(): string | undefined {
  if (typeof window === "undefined") {
    return undefined;
  }

  try {
    return window.sessionStorage.getItem(operatorTokenKey)?.trim() || undefined;
  } catch {
    return undefined;
  }
}

export function setOperatorToken(value: string): void {
  const token = value.trim();
  if (!token) {
    throw new Error("Enter an operator token.");
  }
  window.sessionStorage.setItem(operatorTokenKey, token);
  clientSnapshot = unlockedSnapshot;
  notifySessionListeners();
}

export function lockOperatorSession(
  reason: OperatorLockReason = "manual",
): void {
  if (typeof window !== "undefined") {
    try {
      window.sessionStorage.removeItem(operatorTokenKey);
    } catch {
      // The console still locks when browser storage becomes unavailable.
    }
  }

  clientSnapshot =
    reason === "unauthenticated"
      ? { status: "locked", reason: "unauthenticated" }
      : lockedSnapshot;
  notifySessionListeners();
}

export function clearOperatorLockReason(): void {
  if (clientSnapshot?.status === "locked" && clientSnapshot.reason) {
    clientSnapshot = lockedSnapshot;
    notifySessionListeners();
  }
}

export function getOperatorSessionSnapshot(): OperatorSessionSnapshot {
  if (!clientSnapshot) {
    clientSnapshot = getOperatorToken() ? unlockedSnapshot : lockedSnapshot;
  }
  return clientSnapshot;
}

export function getServerOperatorSessionSnapshot(): OperatorSessionSnapshot {
  return checkingSnapshot;
}

export function subscribeToOperatorSession(listener: () => void): () => void {
  sessionListeners.add(listener);
  return () => sessionListeners.delete(listener);
}
