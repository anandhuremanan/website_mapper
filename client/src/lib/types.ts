// Types mirroring the Go server's JSON API. See server/README.md.

export type ScanStatus = "queued" | "running" | "completed" | "failed" | "cancelled";
export type StepStatus = "pending" | "running" | "done" | "failed" | "skipped" | "stopped";
/** How deep a scan goes; see server/README.md. */
export type ScanMode = "passive" | "light" | "full";

export type Phase =
  | "queued"
  | "discovering_subdomains"
  | "searching_archives"
  | "reading_sitemaps"
  | "resolving_hosts"
  | "probing_hosts"
  | "crawling_hosts"
  | "finalizing"
  | "done";
/** Why a scan ended before finishing its work. */
export type StopReason = "scan_timeout" | "cancelled" | "server_shutdown";

export function isFinished(status: ScanStatus): boolean {
  return status === "completed" || status === "failed" || status === "cancelled";
}

export type Source =
  | "target"
  | "html"
  | "javascript"
  | "sitemap"
  | "robots"
  | "certificate-transparency"
  | "redirect"
  | "host"
  | "archive";

export type UrlType = "page" | "api" | "asset" | "unknown";
export type AssetKind =
  | "javascript"
  | "stylesheet"
  | "image"
  | "font"
  | "media"
  | "document"
  | "other";

export interface Counts {
  hosts: number;
  hostsResolved: number;
  hostsReachable: number;
  hostsCrawled: number;
  hostsUnresolved: number;
  hostsResolvePending: number;
  hostsProbed: number;
  hostsUnreachable: number;
  hostsProbePending: number;
  hostsCrawlPending: number;
  urls: number;
  pages: number;
  apis: number;
  assets: number;
  javascript: number;
  urlsFetched: number;
  urlsFailed: number;
  /** Observations reused from the shared cache instead of made by this scan. */
  cache: { hosts: number; dns: number; probes: number; pages: number };
  limits: {
    hostsOmitted: number;
    resolveSkipped: number;
    probeSkipped: number;
    crawlSkipped: number;
    crawlLimited: number;
    urlsOmitted: number;
  };
}

/** A resource limit that shaped the result. */
export interface LimitNotice {
  code:
    | "scan_timeout"
    | "host_budget_reached"
    | "crawl_limit_reached"
    | "url_budget_reached"
    | "request_budget_reached"
    | "download_budget_reached"
    | "global_resource_wait"
    | "discovery_round_limit";
  message: string;
}

export interface Step {
  id: string;
  label: string;
  status: StepStatus;
  findings: number;
}

export interface EngineError {
  stage?: string;
  engine: string;
  message: string;
  /** The step still produced results (e.g. one provider of several failed). */
  partial?: boolean;
}

export interface Scan {
  id: string;
  target: string;
  domain: string;
  startUrl: string;
  mode: ScanMode;
  status: ScanStatus;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
  durationMs?: number;
  phase: Phase;
  /** 1-based position while queued. */
  queuePosition?: number;
  /** Requesters currently sharing this active scan. */
  subscribers?: number;
  /** Create responses only: this requester's subscription. */
  subscriptionId?: string;
  /** Create responses only: joined an equivalent scan already running. */
  coalesced?: boolean;
  /** Cancel responses only: our subscription was released; others remain. */
  detached?: boolean;
  /** Progress of the current phase, in hosts. */
  progress?: { total: number; completed: number; pending: number };
  stopReason?: StopReason;
  limits: LimitNotice[];
  resources: {
    requests: number;
    maxRequests?: number;
    downloadedBytes?: number;
    maxDownloadBytes?: number;
    /** Per shared pool: operations, how many waited, and the average wait. */
    pools?: Record<string, { operations: number; delayed: number; avgWaitMs: number }>;
  };
  steps: Step[];
  counts: Counts;
  errors: EngineError[];
  error?: string;
}

export type HostState = "discovered" | "resolved" | "reachable";

export interface DnsInfo {
  resolved: boolean;
  addresses?: string[];
  cname?: string;
  nonPublic?: boolean;
  error?: string;
  skipped?: string;
  /** Set when the answer was reused from the cache (time of the lookup). */
  cachedAt?: string;
}

export interface HttpInfo {
  reachable: boolean;
  url?: string;
  scheme?: string;
  status?: number;
  redirect?: string;
  finalUrl?: string;
  finalStatus?: number;
  title?: string;
  server?: string;
  contentType?: string;
  error?: string;
  skipped?: string;
  /** Set when the probe result was reused from the cache. */
  cachedAt?: string;
}

export interface CrawlInfo {
  requests: number;
  /** Pages reused from the page cache (not included in requests). */
  fromCache?: number;
  limitReached?: boolean;
  skipped?: string;
}

export interface SitemapInfo {
  robotsStatus?: number;
  files: number;
  urls: number;
  limitReached?: boolean;
  skipped?: string;
}

export interface Host {
  hostname: string;
  state: HostState;
  /** How the host itself was discovered. */
  sources: Source[];
  /** Discovered only through data reused from the shared cache. */
  fromCache?: boolean;
  dns?: DnsInfo;
  http?: HttpInfo;
  crawl?: CrawlInfo;
  sitemap?: SitemapInfo;
  counts: { urls: number; pages: number; apis: number; assets: number };
  urls: DiscoveredUrl[];
  /** URLs seen on this host but not recorded (per-host limit). */
  urlsOmitted?: number;
}

export interface DiscoveredUrl {
  url: string;
  hostname: string;
  path: string;
  methods?: string[];
  status?: number;
  contentType?: string;
  title?: string;
  redirect?: string;
  server?: string;
  type: UrlType;
  assetKind?: AssetKind;
  typeEvidence: string;
  sources: Source[];
  discoveredFrom?: string[];
  fetched: boolean;
  error?: string;
  /** discovered: referenced only; fetched: requested, no response; verified: responded. */
  state: "discovered" | "fetched" | "verified";
  /** Set when the response was reused from the page cache. */
  cachedAt?: string;
  /** What a web archive recorded (historical; the URL was not requested). */
  archived?: { firstSeen: string; contentType?: string };
}

export interface Technology {
  name: string;
  evidence: string[];
}

export interface ScanResult {
  scanId: string;
  status: ScanStatus;
  stopReason?: StopReason;
  limits: LimitNotice[];
  /** Hostnames discovered after the host limit; not listed. */
  hostsOmitted?: number;
  domain: {
    mode: ScanMode;
    target: string;
    canonical: string;
    startUrl: string;
    scannedAt: string;
    durationMs: number;
  };
  errors: EngineError[];
  counts: Counts;
  hosts: Host[];
  technologies: Technology[];
}
