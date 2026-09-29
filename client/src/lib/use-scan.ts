"use client";

import { useEffect, useState } from "react";
import { ApiError, getResults, getScan } from "./api";
import { isFinished, type Scan, type ScanResult } from "./types";

const POLL_INTERVAL_MS = 1500;

interface ScanState {
  scan: Scan | null;
  result: ScanResult | null;
  error: string | null;
  notFound: boolean;
}

/**
 * Polls a scan's status until it finishes, then loads its results.
 * Polling keeps V1 simple; the hook's shape allows switching to SSE later.
 */
export function useScan(id: string): ScanState {
  const [state, setState] = useState<ScanState>({
    scan: null,
    result: null,
    error: null,
    notFound: false,
  });

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    async function tick() {
      try {
        const scan = await getScan(id);
        if (cancelled) return;
        setState((s) => ({ ...s, scan, error: null }));

        if (isFinished(scan.status)) {
          const result = await getResults(id).catch((err) => {
            // A scan that failed or was cancelled before starting has no results.
            if (err instanceof ApiError && err.status === 409) return null;
            throw err;
          });
          if (!cancelled) setState((s) => ({ ...s, result }));
          return;
        }
      } catch (err) {
        if (cancelled) return;
        if (err instanceof ApiError && err.status === 404) {
          setState((s) => ({ ...s, notFound: true }));
          return;
        }
        // Transient errors: show them but keep polling.
        setState((s) => ({ ...s, error: err instanceof Error ? err.message : String(err) }));
      }
      timer = setTimeout(tick, POLL_INTERVAL_MS);
    }

    tick();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [id]);

  return state;
}
