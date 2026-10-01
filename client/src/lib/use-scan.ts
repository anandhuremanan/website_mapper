"use client";

import { useEffect, useState } from "react";
import { ApiError, getScan, getSummary } from "./api";
import { isFinished, type Scan, type ScanSummary } from "./types";

/**
 * How long to wait before the next status poll. Short scans feel live;
 * long ones are polled less often, which saves the server and the user's
 * connection work for the same information.
 */
export function pollDelay(pollsSoFar: number): number {
  if (pollsSoFar < 20) return 1500; // the first 30 seconds
  if (pollsSoFar < 60) return 3000; // the next two minutes
  return 5000;
}

interface ScanState {
  scan: Scan | null;
  /** The finished scan's result summary; its hosts and URLs are paged. */
  summary: ScanSummary | null;
  error: string | null;
  notFound: boolean;
}

/**
 * Polls a scan's status until it finishes, then loads its result summary.
 * Polling pauses while the tab is hidden and resumes when it is shown.
 */
export function useScan(id: string): ScanState {
  const [state, setState] = useState<ScanState>({
    scan: null,
    summary: null,
    error: null,
    notFound: false,
  });

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let polls = 0;
    let done = false;
    // True when a poll was skipped because the tab was hidden.
    let waiting = false;

    async function tick() {
      // The first request always runs, so a tab opened in the background
      // has its scan ready when it is shown; later polls wait for the tab.
      if (document.hidden && polls > 0) {
        waiting = true;
        return;
      }
      try {
        const scan = await getScan(id);
        if (cancelled) return;
        setState((s) => ({ ...s, scan, error: null }));

        if (isFinished(scan.status)) {
          const summary = await getSummary(id).catch((err) => {
            // A scan that failed or was cancelled before starting has no results.
            if (err instanceof ApiError && err.status === 409) return null;
            throw err;
          });
          done = true;
          if (!cancelled) setState((s) => ({ ...s, summary }));
          return;
        }
      } catch (err) {
        if (cancelled) return;
        if (err instanceof ApiError && err.status === 404) {
          done = true;
          setState((s) => ({ ...s, notFound: true }));
          return;
        }
        // Transient errors: show them but keep polling.
        setState((s) => ({ ...s, error: err instanceof Error ? err.message : String(err) }));
      }
      timer = setTimeout(tick, pollDelay(polls++));
    }

    function onVisible() {
      if (!document.hidden && waiting && !done && !cancelled) {
        waiting = false;
        void tick();
      }
    }

    document.addEventListener("visibilitychange", onVisible);
    void tick();
    return () => {
      cancelled = true;
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [id]);

  return state;
}
