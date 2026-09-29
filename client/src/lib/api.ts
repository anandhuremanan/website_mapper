import type { Scan, ScanResult } from "./types";

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    res = await fetch(path, {
      ...init,
      headers: { "Content-Type": "application/json", ...init?.headers },
      cache: "no-store",
    });
  } catch {
    throw new ApiError("Could not reach the server. Is it running?", 0);
  }
  const body = await res.json().catch(() => null);
  if (!res.ok) {
    const message =
      body && typeof body.error === "string" ? body.error : `Request failed (${res.status})`;
    throw new ApiError(message, res.status);
  }
  return body as T;
}

// Subscriptions identify this browser tab's interest in a (possibly shared)
// scan, so cancelling only releases our own subscription.
const subscriptionKey = (scanId: string) => `scan-subscription:${scanId}`;

function rememberSubscription(scan: Scan) {
  if (!scan.subscriptionId) return;
  try {
    sessionStorage.setItem(subscriptionKey(scan.id), scan.subscriptionId);
  } catch {
    // Storage unavailable: cancelling a shared scan will be refused.
  }
}

function subscriptionFor(scanId: string): string | null {
  try {
    return sessionStorage.getItem(subscriptionKey(scanId));
  } catch {
    return null;
  }
}

export async function createScan(target: string): Promise<Scan> {
  const scan = await request<Scan>("/api/scans", {
    method: "POST",
    body: JSON.stringify({ target }),
  });
  rememberSubscription(scan);
  return scan;
}

export function getScan(id: string): Promise<Scan> {
  return request<Scan>(`/api/scans/${encodeURIComponent(id)}`);
}

/**
 * Releases this tab's subscription to a scan. The scan stops only when no
 * other requester shares it; otherwise the response has `detached: true`.
 */
export function cancelScan(id: string): Promise<Scan> {
  const subscriptionId = subscriptionFor(id);
  return request<Scan>(`/api/scans/${encodeURIComponent(id)}/cancel`, {
    method: "POST",
    body: subscriptionId ? JSON.stringify({ subscriptionId }) : undefined,
  });
}

export function getResults(id: string): Promise<ScanResult> {
  return request<ScanResult>(`/api/scans/${encodeURIComponent(id)}/results`);
}
