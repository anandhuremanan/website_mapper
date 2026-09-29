# Website Mapper — server

Go HTTP API that runs passive discovery scans against a public domain. It
finds the domain's hosts, checks which ones resolve and answer HTTP(S),
crawls the reachable ones, and returns a normalized map of hosts and their
URLs, with provenance for every result.

Requires Go 1.23+. The only third-party dependency is `golang.org/x/net/html`.

## Commands

```sh
go run ./cmd/server           # run on :8080 (see .env.example for settings)
go test ./...                 # tests (deterministic, no network access)
go vet ./...
gofmt -l .                    # list unformatted files (should print nothing)
go build -o bin/server ./cmd/server
```

Configuration comes from environment variables only; every variable and its
default is listed in [.env.example](.env.example).

## Concepts

A scan keeps three things apart:

- **Target domain.** What the user entered. It defines the scope: the domain
  itself (without a leading `www.`) and all of its subdomains.
- **Hosts.** Hostnames in scope, from any source. Each host moves through
  three states: *discovered* (the name was seen), *resolved* (it has A/AAAA
  records), and *reachable* (it answered HTTP or HTTPS).
- **URLs.** Routes, API-like endpoints and assets. Each URL belongs to one
  host.

Subdomain discovery (which hosts exist) and route discovery (what is on a
host) are separate engines.

## Architecture

```text
cmd/server            wiring: config → fetch clients → engines → stages → service → HTTP
internal/api          REST handlers (net/http ServeMux)
internal/scan         scan model, stage pipeline, worker pool, Repository interface
internal/store        in-memory Repository (swap for PostgreSQL later)
internal/discovery    Engine interface, Finding, State, Target/scope
  subdomains          passive hostname discovery; Source providers: crt.sh, Cert Spotter
  dnsresolve          resolves hosts (injectable Resolver)
  httpprobe           checks https:// then http:// on resolved hosts
  htmlcrawl           crawls reachable hosts, each with its own budget
internal/results      Aggregator: merges findings into hosts → URLs, implements State
internal/normalize    URL and hostname canonicalization (deduplication keys)
internal/classify     page / api / asset / unknown, always with evidence
internal/fetch        outbound HTTP: timeouts, size caps, per-host rate limit,
                      no auto-redirects, non-public address guard
internal/htmlmeta     <title> extraction
internal/config       environment configuration
```

### Pipeline

The scan runs an ordered list of stages, defined in `pipeline()` in
[cmd/server/main.go](cmd/server/main.go). Each stage is one progress step.

| Step | Engines | What happens |
| --- | --- | --- |
| Validating target | (built in) | Input parsed and scope derived when the scan is created |
| Discovering subdomains | `subdomains` | Certificate Transparency via crt.sh and Cert Spotter, run concurrently and merged |
| Resolving discovered hosts | `dns` | A/AAAA/CNAME lookup for every known host |
| Probing hosts | `http` | `GET https://host/`, falling back to `http://host/`, on resolved public hosts |
| Crawling reachable hosts | `html` | BFS crawl per reachable host |
| Checking hosts found while crawling | `dns`, `http`, `html` | The same engines again, for hosts first seen in links or redirects |
| Finalizing results | (built in) | Aggregate and store |

Before the first stage, the entered host and the apex domain are added as
hosts, so they are always resolved, probed and crawled.

Engines read a shared, read-only `discovery.State` (the hosts and URLs found
so far) and report `Finding`s. Engines that act on hosts only process hosts
they have not handled yet, so the scan can run them again in the follow-up
stage. To add an engine (robots.txt, sitemap, JavaScript, …), implement
`discovery.Engine` and insert it into a stage or add a new stage. The service
does not change.

```go
type Engine interface {
    Name() string
    Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error
}
```

To add a passive hostname provider, implement `subdomains.Source`
(`Name`, `Provenance`, `Discover(ctx, domain) ([]string, error)`) and add it
to the `sources` list. The engine normalizes names (case, trailing dot, `*.`
prefix), drops anything outside the scope (`evil-example.com` is not part of
`example.com`), and fails only if every provider fails.

### Budgets

- `SCAN_MAX_RESOLVE_HOSTS` bounds how many hosts are resolved,
  `SCAN_MAX_PROBE_HOSTS` how many are probed, and `SCAN_MAX_HOSTS` how many
  are crawled. The order is deterministic: the entered host, then the apex,
  then www, then hosts linked from content, then hosts only seen in
  certificates. A host that falls outside a limit is reported with a
  `skipped` reason, not dropped.
- Each crawled host gets its own request budget (`SCAN_MAX_URLS`) and depth
  (`SCAN_MAX_DEPTH`), so one large host cannot use up the scan. Hosts whose
  root answers 2xx are crawled before those answering 3xx/4xx/5xx.
- A host whose root only redirects to another host (for example
  `blog.example.com` → `example.com/blog`) is recorded from its probe. It
  isn't crawled and doesn't use a crawl slot.
- `SCAN_MAX_RECORDED_URLS_PER_HOST` caps the URLs kept per host. Pages that
  link thousands of URLs would otherwise make results huge. The rest are
  counted in `urlsOmitted`.

### Responsible use

- **Consent.** `POST /api/scans` requires `"authorizationConfirmed": true`:
  the caller confirms they own the domain or have permission to scan it.
  The check is in the scan service, so it applies to direct API calls as
  well as the UI's checkbox. Rejected attempts are logged. The confirmation
  is recorded on the scan.
- **Identification.** Every request sends a recognizable User-Agent,
  `WebsiteMapperBot/0.1 (+<BOT_INFO_URL>)`, which links to the client's
  `/bot` page. That page describes the crawler, its limits and the operator
  contact (`BOT_CONTACT`), using `GET /api/bot`. The server logs a warning
  at startup when `BOT_INFO_URL` or `BOT_CONTACT` is not set.
- **robots.txt** is respected by default (`SCAN_RESPECT_ROBOTS`). Before
  crawling a host, the crawler fetches its `/robots.txt` (RFC 9309:
  a group for our product token takes precedence over `*`, the longest
  match wins, and `*`/`$` patterns are supported) and never requests
  disallowed URLs. Links to disallowed URLs are still reported as found,
  marked `robotsDisallowed`. `Disallow` lines themselves are never treated as
  routes. If robots.txt cannot be fetched, the crawl continues normally.
  The probe's single request to a host's root happens before robots.txt is
  read.
- **Request limits.** Each scan has a total request budget
  (`SCAN_MAX_REQUESTS`) and an overall rate limit
  (`SCAN_MAX_REQUESTS_PER_SECOND`), carried in the scan's context and
  enforced by the shared HTTP client for every engine. These come on top of
  the per-host rate, per-host request budget and host limits. Scans report
  `requests`; hitting the budget adds a notice and leaves partial results.
- **Only metadata is stored.** Response bodies are read into memory to
  extract links and titles, then discarded. Results hold URL, hostname,
  status, content type, title, redirect target, `Server` header, discovery
  source, timestamps and crawl metadata. No other response headers
  (including `Set-Cookie`) are stored.
- **No credentials.** Requests carry no cookies (there is no cookie jar),
  no `Authorization` header and no body, and URL user info is dropped
  before sending. Before storage, user info is removed from URLs and the
  values of sensitive query parameters (`token`, `password`, `api_key`,
  `session`, `sig`, `X-Amz-Signature`, …) are replaced with `REDACTED`.

### Scope and safety

- A target is a public hostname. IP addresses, custom ports and local names
  are rejected.
- Only in-scope hosts are resolved, probed or crawled. A crawl fetches only
  URLs on the host it is crawling; links to other in-scope hosts register
  those hosts for their own crawl. Links to other domains are ignored.
  Redirects are followed only within the same host during a crawl, and only
  within scope during a probe.
- Requests are GET only. Forms are recorded, never submitted. Asset URLs are
  recorded without being downloaded. There is no DNS brute forcing, no
  wordlists and no port scanning.
- **Address checks run at connect time.** The HTTP client's dialer checks
  each IP it is about to connect to, after DNS resolution. Every connection
  is checked this way, so a hostname that re-resolves (DNS rebinding) cannot
  bypass the policy. Loopback, private (RFC 1918, ULA), link-local (including
  cloud metadata at 169.254.169.254), CGNAT, multicast, documentation,
  benchmarking and reserved ranges are refused. So are IPv4-mapped, NAT64,
  6to4 and Teredo addresses that embed such IPv4 addresses. Hosts whose DNS
  answers are all non-public are flagged `nonPublic` and never probed.

## API

All responses are JSON. Errors look like `{"error": "message"}`.

### `GET /api/health`

```json
{ "status": "ok" }
```

### `GET /api/bot`

Describes the crawler for the `/bot` page: `name`, `userAgent`,
`robotsToken`, `infoUrl`, `contact`, `respectsRobotsTxt` and `limits`
(`requestsPerSecondPerHost`, `requestsPerSecondPerScan`,
`maxRequestsPerScan`, `maxRequestsPerHost`, `maxHostsCrawled`,
`requestTimeoutSeconds`).

### `POST /api/scans`

Request:

```json
{ "target": "example.com", "authorizationConfirmed": true }
```

`target` can be a bare domain or a URL such as `https://www.example.com/docs`.
`authorizationConfirmed` is required and must be `true`. It confirms that the
caller owns the domain or has permission to scan it.

Response `202 Accepted` (with a `Location` header) contains the scan status
object described below, with `"status": "queued"`.

Errors: `400` for a missing or false `authorizationConfirmed`, an invalid
target or an invalid body, and `503` with `Retry-After` when the queue is
full.

### `GET /api/scans/{id}`

Returns the scan status. Poll this every 1–2 seconds while a scan runs.

```json
{
  "id": "3f9c2a1b7d4e5f60",
  "target": "example.com",
  "domain": "example.com",
  "startUrl": "https://example.com/",
  "status": "running",
  "createdAt": "2026-09-29T06:54:02Z",
  "startedAt": "2026-09-29T06:54:02Z",
  "steps": [
    { "id": "validate", "label": "Validating target", "status": "done", "findings": 0 },
    { "id": "subdomains", "label": "Discovering subdomains", "status": "done", "findings": 26 },
    { "id": "resolve", "label": "Resolving discovered hosts", "status": "running", "findings": 0 },
    { "id": "probe", "label": "Probing hosts", "status": "pending", "findings": 0 },
    { "id": "crawl", "label": "Crawling reachable hosts", "status": "pending", "findings": 0 },
    { "id": "follow-up", "label": "Checking hosts found while crawling", "status": "pending", "findings": 0 },
    { "id": "finalize", "label": "Finalizing results", "status": "pending", "findings": 0 }
  ],
  "counts": {
    "hosts": 15, "hostsResolved": 11, "hostsReachable": 0, "hostsCrawled": 0,
    "urls": 0, "pages": 0, "apis": 0, "assets": 0, "javascript": 0
  },
  "requests": 57,
  "authorizationConfirmed": true,
  "errors": []
}
```

- `status`: `queued` | `running` | `completed` | `failed`
- `requests`: outbound HTTP requests made so far
- `steps[].status`: `pending` | `running` | `done` | `failed` | `skipped`
- `errors`: engine failures, e.g.
  `{ "stage": "subdomains", "engine": "subdomains", "message": "crt.sh: …; certspotter: …" }`.
  A `completed` scan can still have errors; its results are then partial.
- `error`: set only when the whole scan failed.
- `finishedAt` and `durationMs` are set once the scan finishes.

`404` if the scan does not exist. Scans live in memory and are lost on restart.

### `GET /api/scans/{id}/results`

Available once the scan is `completed` or `failed`. Returns `409` with
`{"error": "...", "status": "running"}` before that.

The result has a hierarchy: `hosts[]`, each with its own `urls[]`.

```json
{
  "scanId": "3f9c2a1b7d4e5f60",
  "status": "completed",
  "domain": {
    "target": "example.com",
    "canonical": "example.com",
    "startUrl": "https://example.com/",
    "scannedAt": "2026-09-29T06:54:02Z",
    "durationMs": 14464
  },
  "errors": [],
  "counts": { "hosts": 3, "hostsResolved": 2, "hostsReachable": 2, "hostsCrawled": 2, "urls": 2, "pages": 1, "apis": 1, "assets": 0, "javascript": 0 },
  "hosts": [
    {
      "hostname": "api.example.com",
      "state": "reachable",
      "sources": ["certificate-transparency"],
      "dns": { "resolved": true, "addresses": ["203.0.113.10"], "cname": "edge.provider.net" },
      "http": {
        "reachable": true, "url": "https://api.example.com/", "scheme": "https",
        "status": 200, "finalUrl": "https://api.example.com/", "finalStatus": 200,
        "title": "API", "server": "nginx", "contentType": "text/html"
      },
      "crawl": { "requests": 4 },
      "counts": { "urls": 2, "pages": 1, "apis": 1, "assets": 0 },
      "urls": [
        {
          "url": "https://api.example.com/v1/projects",
          "hostname": "api.example.com",
          "path": "/v1/projects",
          "type": "api",
          "typeEvidence": "Path contains /v1/ (not verified by a response)",
          "sources": ["html"],
          "discoveredFrom": ["https://api.example.com/"],
          "fetched": false
        }
      ]
    },
    {
      "hostname": "old.example.com",
      "state": "discovered",
      "sources": ["certificate-transparency"],
      "dns": { "resolved": false, "error": "no such host" },
      "counts": { "urls": 0, "pages": 0, "apis": 0, "assets": 0 },
      "urls": []
    }
  ],
  "technologies": [
    { "name": "nginx", "evidence": ["Server: nginx"] }
  ]
}
```

Host fields:

| Field | Meaning |
| --- | --- |
| `state` | `discovered`, `resolved` or `reachable` (see Concepts) |
| `sources` | How the host was discovered: `target`, `certificate-transparency`, `html`, `redirect`, … |
| `dns` | Absent if not checked. `resolved`, `addresses`, `cname`, `nonPublic` (all addresses private/reserved, never contacted), `error`, or `skipped` (limit reached) |
| `http` | Absent if not probed. `reachable` means any HTTP response, including 4xx/5xx. `status` and `redirect` come from the root URL; `finalUrl`, `finalStatus` and `title` come from the end of an in-scope redirect chain. Also `error` or `skipped` |
| `crawl` | `requests`, `limitReached`, `robots` (`respected`, `not found`, `unavailable` or `ignored`), `robotsDisallowed` (URLs not requested), or `skipped` with a reason |
| `urlsOmitted` | URLs seen on the host beyond the per-host recording limit |

URL fields:

| Field | Meaning |
| --- | --- |
| `url` | Normalized URL: lowercase host, no default port, fragment or trailing slash, sorted query, tracking parameters removed |
| `type` | `page`, `api`, `asset` or `unknown` |
| `assetKind` | For assets: `javascript`, `stylesheet`, `image`, `font`, `media`, `document`, `other` |
| `typeEvidence` | Why `type` was chosen. A server response beats how the URL was referenced, which beats its file extension, which beats its path pattern |
| `sources` | Every way the URL was discovered: `target`, `host` (root of a discovered host), `html`, `redirect` |
| `fetched` | Whether the scanner requested the URL. Unfetched URLs have no `status` |
| `methods` | Methods seen in context, e.g. `POST` for a form target |
| `status`, `contentType`, `title`, `redirect`, `server` | Response metadata when fetched |
| `discoveredFrom` | Up to 5 pages that referenced the URL |
| `error` | Why a request for this URL failed |
| `robotsDisallowed` | Not requested because the host's robots.txt disallows it |

## Notes on data sources

- **crt.sh** returns the full certificate history, which includes names that
  no longer resolve. It is slow and often returns 502/503 under load, so it
  is retried twice.
- **Cert Spotter** returns only current certificates. Unauthenticated use is
  limited to about 10 requests per hour per IP; each scan uses up to 3.
- Neither source is a complete DNS inventory. Hosts that never had their own
  certificate (for example ones covered only by a wildcard) will not appear
  unless they are linked from a crawled page.
