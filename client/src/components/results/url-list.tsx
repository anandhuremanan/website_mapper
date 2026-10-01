"use client";

import { useMemo, useState } from "react";
import { SourceTags } from "@/components/source-list";
import { assetKindLabels, statusTone, typeLabels } from "@/lib/format";
import type { DiscoveredUrl } from "@/lib/types";
import { UrlDetail } from "./url-detail";

const PAGE_SIZE = 200;

interface Props {
  urls: DiscoveredUrl[];
  empty: string;
  /** Show the asset kind (or type) column. */
  showKind?: boolean;
  note?: React.ReactNode;
}

export function UrlList({ urls, empty, showKind, note }: Props) {
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState<string | null>(null);
  const [limit, setLimit] = useState(PAGE_SIZE);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return urls;
    return urls.filter(
      (u) => u.url.toLowerCase().includes(q) || u.title?.toLowerCase().includes(q),
    );
  }, [urls, query]);

  if (urls.length === 0) {
    return <p className="text-sm text-muted">{empty}</p>;
  }

  return (
    <div className="space-y-3">
      {note && <p className="text-sm text-muted">{note}</p>}
      <input
        type="search"
        placeholder={`Filter ${urls.length.toLocaleString()} URLs`}
        value={query}
        onChange={(e) => {
          setQuery(e.target.value);
          setLimit(PAGE_SIZE);
        }}
        className="h-9 w-full rounded-md border border-border bg-surface px-3 text-sm outline-none focus:border-accent sm:w-80"
      />

      <ul className="divide-y divide-border rounded-md border border-border bg-surface">
        {filtered.slice(0, limit).map((u) => {
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
        {filtered.length === 0 && (
          <li className="px-3 py-4 text-sm text-muted">No URLs match “{query}”.</li>
        )}
      </ul>

      {filtered.length > limit && (
        <button
          type="button"
          onClick={() => setLimit((l) => l + PAGE_SIZE)}
          className="text-sm text-accent hover:underline"
        >
          Show more ({(filtered.length - limit).toLocaleString()} remaining)
        </button>
      )}
    </div>
  );
}
