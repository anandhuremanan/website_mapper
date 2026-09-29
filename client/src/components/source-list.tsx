import { sourceLabel } from "@/lib/format";

/** Compact provenance tags, e.g. "HTML · Sitemap". */
export function SourceTags({ sources }: { sources: string[] }) {
  return (
    <span className="flex flex-wrap gap-1">
      {sources.map((s) => (
        <span
          key={s}
          className="rounded border border-border px-1.5 py-px text-[11px] leading-4 text-muted"
        >
          {sourceLabel(s)}
        </span>
      ))}
    </span>
  );
}

/** Long-form provenance: "Found through: ✓ HTML ✓ Sitemap". */
export function FoundThrough({ sources }: { sources: string[] }) {
  return (
    <ul className="space-y-0.5">
      {sources.map((s) => (
        <li key={s}>
          <span className="mr-1.5 text-ok">✓</span>
          {sourceLabel(s)}
        </li>
      ))}
    </ul>
  );
}
