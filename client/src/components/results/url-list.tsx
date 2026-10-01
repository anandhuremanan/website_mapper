"use client";

import { useState } from "react";
import { SourceTags } from "@/components/source-list";
import { getUrls } from "@/lib/api";
import { assetKindLabels, statusTone, typeLabels } from "@/lib/format";
import type { UrlType } from "@/lib/types";
import { useDebounced, usePages } from "@/lib/use-pages";
import { UrlDetail } from "./url-detail";

interface Props {
  scanId: string;
  /** List one host's URLs; every host's when omitted. */
  host?: string;
  /** URL types to list; every type when omitted. */
  types?: UrlType[];
  /** How many URLs the listing has in total (from the result's counts). */
  total: number;
  empty: string;
  /** Show the asset kind (or type) column. */
  showKind?: boolean;
  note?: React.ReactNode;
}

/**
 * A searchable list of URLs, read from the server a page at a time, so it
 * works the same for a hundred URLs or a million.
 */
export function UrlList({ total, empty, note, ...listing }: Props) {
  const [query, setQuery] = useState("");
  const search = useDebounced(query.trim());

  if (total === 0) {
    return <p className="text-sm text-muted">{empty}</p>;
  }

  return (
    <div className="space-y-3">
      {note && <p className="text-sm text-muted">{note}</p>}
      <input
        type="search"
        placeholder={`Search ${total.toLocaleString()} URLs`}
        aria-label="Search URLs"
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        className="h-9 w-full rounded-md border border-border bg-surface px-3 text-sm outline-none focus:border-accent sm:w-80"
      />
      {/* A new search is a new listing: the key starts it from its first page. */}
      <UrlRows key={search} search={search} {...listing} />
    </div>
  );
}

type RowsProps = Omit<Props, "total" | "empty" | "note"> & { search: string };

function UrlRows({ scanId, host, types, showKind, search }: RowsProps) {
  const [open, setOpen] = useState<string | null>(null);
  const { pages, loading, error, hasMore, loadMore } = usePages((after) =>
    getUrls(scanId, { host, types, q: search, after }),
  );
  const urls = pages.flatMap((p) => p.urls);

  return (
    <>
      <ul className="divide-y divide-border rounded-md border border-border bg-surface">
        {urls.map((u) => {
          const isOpen = open === u.url;
          return (
            <li key={u.url}>
              <button
                type="button"
                onClick={() => setOpen(isOpen ? null : u.url)}
                aria-expanded={isOpen}
                className="grid w-full grid-cols-[3rem_minmax(0,1fr)] items-center gap-x-3 gap-y-1 px-3 py-2 text-left text-sm hover:bg-subtle sm:grid-cols-[3rem_minmax(0,1fr)_auto]"
              >
                <span className={`font-mono tabular-nums ${statusTone(u.status)}`}>
                  {u.status ?? "—"}
                </span>
                <span className="min-w-0 truncate font-mono">
                  <span className="text-muted">{u.hostname}</span>
                  {u.path}
                  {u.url.includes("?") && (
                    <span className="text-muted">{u.url.slice(u.url.indexOf("?"))}</span>
                  )}
                </span>
                <span className="col-start-2 flex items-center gap-2 sm:col-start-auto">
                  {showKind && (
                    <span className="text-xs text-muted">
                      {u.assetKind ? assetKindLabels[u.assetKind] : typeLabels[u.type]}
                    </span>
                  )}
                  <SourceTags sources={u.sources} />
                </span>
              </button>
              {isOpen && (
                <div className="border-t border-border bg-subtle/60 px-3 py-4">
                  <UrlDetail url={u} />
                </div>
              )}
            </li>
          );
        })}
        {urls.length === 0 && !loading && !error && (
          <li className="px-3 py-4 text-sm text-muted">
            {search ? `No URLs match “${search}”.` : "No URLs to show."}
          </li>
        )}
        {urls.length === 0 && loading && (
          <li className="px-3 py-4 text-sm text-muted">Loading…</li>
        )}
      </ul>

      <ListFooter
        shown={urls.length}
        loading={loading}
        error={error}
        hasMore={hasMore}
        onMore={loadMore}
      />
    </>
  );
}

interface FooterProps {
  /** Rows already shown. */
  shown: number;
  loading: boolean;
  error: string | null;
  hasMore: boolean;
  onMore: () => void;
}

/** The "Show more" button of a paged list, with its loading and error states. */
export function ListFooter({ shown, loading, error, hasMore, onMore }: FooterProps) {
  if (error) {
    return (
      <p role="alert" className="text-sm text-warn">
        Could not load {shown > 0 ? "more" : "the list"}: {error}{" "}
        <button type="button" onClick={onMore} className="text-accent hover:underline">
          Try again
        </button>
      </p>
    );
  }
  if (!hasMore || shown === 0) return null;
  return (
    <button
      type="button"
      onClick={onMore}
      disabled={loading}
      className="text-sm text-accent hover:underline disabled:opacity-50"
    >
      {loading ? "Loading…" : "Show more"}
    </button>
  );
}
