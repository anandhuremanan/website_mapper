import type { AssetKind, Host, ScanMode, Source, UrlType } from "./types";

export const sourceLabels: Record<Source, string> = {
  target: "Entered by you",
  html: "HTML",
  javascript: "JavaScript",
  sitemap: "Sitemap",
  robots: "robots.txt",
  "certificate-transparency": "Certificate Transparency",
  redirect: "Redirect",
  host: "Discovered host",
  archive: "Web archive",
  dataset: "Subdomain database",
};

export const modeLabels: Record<ScanMode, string> = {
  passive: "Passive",
  light: "Standard",
  full: "Full crawl",
};

export function sourceLabel(s: string): string {
  return sourceLabels[s as Source] ?? s;
}

export const typeLabels: Record<UrlType, string> = {
  page: "Page",
  api: "API-like",
  asset: "Asset",
  unknown: "Unknown",
};

export const assetKindLabels: Record<AssetKind, string> = {
  javascript: "JavaScript",
  stylesheet: "CSS",
  image: "Image",
  font: "Font",
  media: "Media",
  document: "Document",
  other: "Other",
};

/** Tailwind classes for an HTTP status, neutral by default. */
export function statusTone(status?: number): string {
  if (!status) return "text-muted";
  if (status < 300) return "text-ok";
  if (status < 400) return "text-info";
  return "text-warn";
}

export function formatDuration(ms?: number): string {
  if (!ms) return "—";
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)} s`;
  return `${Math.floor(s / 60)} min ${Math.round(s % 60)} s`;
}

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n.toLocaleString()} ${n === 1 ? one : many}`;
}

/** Strips the scheme for compact display. */
export function shortUrl(url: string): string {
  return url.replace(/^https?:\/\//, "");
}

export interface HostStatus {
  icon: string;
  tone: string;
  /** Short status, e.g. "200 · HTTPS". */
  label: string;
  /** One-sentence explanation. */
  detail: string;
}

/** Summarizes how far a host got: resolving, answering HTTP, and how. */
export function hostStatus(h: Host): HostStatus {
  const { dns, http } = h;
  if (h.state === "reachable" && http) {
    const scheme = http.scheme?.toUpperCase() ?? "";
    const redirect = http.redirect ? ` → ${shortUrl(http.redirect)}` : "";
    return {
      icon: "✓",
      tone: "text-ok",
      label: [http.status, scheme].filter(Boolean).join(" · "),
      detail: `Answered HTTP${scheme === "HTTPS" ? "S" : ""} with ${http.status}${redirect}.`,
    };
  }
  if (h.state === "resolved") {
    if (dns?.nonPublic) {
      return {
        icon: "?",
        tone: "text-warn",
        label: "private address",
        detail: "Resolves only to private or reserved addresses, so it was not contacted.",
      };
    }
    if (!http) {
      // Passive scans never contact hosts, so there is no HTTP answer to report.
      return {
        icon: "✓",
        tone: "text-muted",
        label: "resolves",
        detail: "Resolves in DNS. Not contacted: this scan did not visit hosts.",
      };
    }
    if (http?.skipped) {
      return { icon: "?", tone: "text-muted", label: "not probed", detail: `Resolves; not probed (${http.skipped}).` };
    }
    return {
      icon: "?",
      tone: "text-warn",
      label: "no HTTP response",
      detail: `Resolves, but did not answer HTTP or HTTPS${http?.error ? ` (${http.error})` : ""}.`,
    };
  }
  if (dns && !dns.resolved && !dns.skipped) {
    return {
      icon: "○",
      tone: "text-muted",
      label: "does not resolve",
      detail: `Discovered, but the name does not currently resolve${dns.error ? ` (${dns.error})` : ""}.`,
    };
  }
  return {
    icon: "○",
    tone: "text-muted",
    label: "not checked",
    detail: `Discovered, not checked${dns?.skipped ? ` (${dns.skipped})` : ""}.`,
  };
}
