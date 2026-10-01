import type {
  HostPage,
  Scan,
  ScanMode,
  ScanSummary,
  TreeLevel,
  UrlPage,
  UrlType,
} from "./types";

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

export async function createScan(target: string, mode: ScanMode): Promise<Scan> {
  const scan = await request<Scan>("/api/scans", {
    method: "POST",
    body: JSON.stringify({ target, mode }),
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

// A finished scan's result is read in pages: a summary, then hosts, URLs
// and tree levels as the user looks at them. Each page's `next` is passed
// back as `after` to get the following page.

function resultPath(id: string, part: string, params: Record<string, string | undefined> = {}) {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value) query.set(key, value);
  }
  const qs = query.toString();
  return `/api/scans/${encodeURIComponent(id)}/${part}${qs ? `?${qs}` : ""}`;
}

export function getSummary(id: string): Promise<ScanSummary> {
  return request<ScanSummary>(resultPath(id, "summary"));
}

export interface HostQuery {
  /** Keeps hosts whose name contains this text. */
  q?: string;
  after?: string;
}

export function getHosts(id: string, query: HostQuery = {}): Promise<HostPage> {
  return request<HostPage>(resultPath(id, "hosts", { q: query.q, after: query.after }));
}

export interface UrlQuery {
  /** One host; every host when omitted. */
  host?: string;
  /** URL types to keep; every type when omitted. */
  types?: UrlType[];
  /** Keeps URLs whose path, title or (across hosts) hostname contains this. */
  q?: string;
  after?: string;
}

export function getUrls(id: string, query: UrlQuery = {}): Promise<UrlPage> {
  return request<UrlPage>(
    resultPath(id, "urls", {
      host: query.host,
      type: query.types?.join(","),
      q: query.q,
      after: query.after,
    }),
  );
}

export interface TreeQuery {
  host: string;
  /** The parent path; the host's root when omitted. */
  path?: string;
  /** Include asset URLs (scripts, styles, images, fonts). */
  assets?: boolean;
  after?: string;
}

export function getTree(id: string, query: TreeQuery): Promise<TreeLevel> {
  return request<TreeLevel>(
    resultPath(id, "tree", {
      host: query.host,
      path: query.path,
      assets: query.assets ? "true" : undefined,
      after: query.after,
    }),
  );
}

/** Address of a CSV file with every URL of a result. */
export function exportUrl(id: string): string {
  return resultPath(id, "export");
}
