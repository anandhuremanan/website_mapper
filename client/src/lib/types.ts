// Types mirroring the Go server's JSON API. See server/README.md.

export type ScanStatus = "queued" | "running" | "completed" | "failed";
export type StepStatus = "pending" | "running" | "done" | "failed" | "skipped";

export type Source =
  | "target"
  | "html"
  | "javascript"
  | "sitemap"
  | "robots"
  | "certificate-transparency"
  | "redirect"
  | "host";

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
  urls: number;
  pages: number;
  apis: number;
  assets: number;
  javascript: number;
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
}

export interface Scan {
  id: string;
  target: string;
  domain: string;
  startUrl: string;
  status: ScanStatus;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
  durationMs?: number;
  steps: Step[];
  counts: Counts;
  /** Outbound HTTP requests made so far. */
  requests: number;
  authorizationConfirmed: boolean;
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
}

export interface CrawlInfo {
  requests: number;
  limitReached?: boolean;
  skipped?: string;
  /** "respected" | "not found" | "unavailable" | "ignored" */
  robots?: string;
  robotsDisallowed?: number;
}

export interface Host {
  hostname: string;
  state: HostState;
  /** How the host itself was discovered. */
  sources: Source[];
  dns?: DnsInfo;
  http?: HttpInfo;
  crawl?: CrawlInfo;
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
  /** Not requested because the host's robots.txt disallows it. */
  robotsDisallowed?: boolean;
}

export interface BotInfo {
  name: string;
  userAgent: string;
  robotsToken: string;
  infoUrl: string;
  contact?: string;
  respectsRobotsTxt: boolean;
  limits: {
    requestsPerSecondPerHost: number;
    requestsPerSecondPerScan: number;
    maxRequestsPerScan: number;
    maxRequestsPerHost: number;
    maxHostsCrawled: number;
    requestTimeoutSeconds: number;
  };
}

export interface Technology {
  name: string;
  evidence: string[];
}

export interface ScanResult {
  scanId: string;
  status: ScanStatus;
  domain: {
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
