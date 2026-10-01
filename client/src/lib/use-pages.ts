"use client";

import { useCallback, useEffect, useRef, useState } from "react";

interface Paged {
  next?: string;
}

interface Pages<P> {
  /** Pages loaded so far, in order. */
  pages: P[];
  /** True while a page is being loaded. */
  loading: boolean;
  error: string | null;
  /** True when the server has more pages. */
  hasMore: boolean;
  /** Loads the next page (or retries after an error). */
  loadMore: () => void;
}

/**
 * Loads a listing one page at a time: the first page when `enabled`, then
 * one more for each `loadMore` call.
 *
 * `load` must describe one listing for the lifetime of the component. To
 * show a different listing (another search, another host), give the
 * component using this hook a different `key` so it starts again.
 */
export function usePages<P extends Paged>(
  load: (after?: string) => Promise<P>,
  enabled = true,
): Pages<P> {
  const [pages, setPages] = useState<P[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The latest `load`, and whether a request is in flight, without
  // re-creating `loadMore` on every render.
  const loadRef = useRef(load);
  const busy = useRef(false);
  const started = useRef(false);
  const next = useRef<string | undefined>(undefined);
  const alive = useRef(true);

  useEffect(() => {
    loadRef.current = load;
  });
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  const loadMore = useCallback(async () => {
    if (busy.current) return;
    busy.current = true;
    setLoading(true);
    setError(null);
    try {
      const page = await loadRef.current(next.current);
      if (!alive.current) return;
      next.current = page.next;
      setPages((prev) => [...prev, page]);
    } catch (err) {
      if (alive.current) setError(err instanceof Error ? err.message : String(err));
    } finally {
      busy.current = false;
      if (alive.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (!enabled || started.current) return;
    started.current = true;
    void loadMore();
  }, [enabled, loadMore]);

  const last = pages[pages.length - 1];
  return {
    pages,
    // Before the first page arrives the listing counts as loading.
    loading: loading || (enabled && pages.length === 0 && !error),
    error,
    hasMore: Boolean(last?.next),
    loadMore: () => void loadMore(),
  };
}

/** Returns `value` once it has stopped changing for `ms` milliseconds. */
export function useDebounced<T>(value: T, ms = 300): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(timer);
  }, [value, ms]);
  return settled;
}
