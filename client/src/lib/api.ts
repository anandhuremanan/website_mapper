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

/**
 * Starts a scan. The server rejects the request unless
 * authorizationConfirmed is true.
 */
export function createScan(target: string, authorizationConfirmed: boolean): Promise<Scan> {
  return request<Scan>("/api/scans", {
    method: "POST",
    body: JSON.stringify({ target, authorizationConfirmed }),
  });
}

export function getScan(id: string): Promise<Scan> {
  return request<Scan>(`/api/scans/${encodeURIComponent(id)}`);
}

export function getResults(id: string): Promise<ScanResult> {
  return request<ScanResult>(`/api/scans/${encodeURIComponent(id)}/results`);
}
