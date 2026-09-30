# Web Scanner — server

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
internal/scan         scan model, scheduler (queue, cancellation, shutdown),
                      stage pipeline, Repository interface
internal/resource     shared resource pools with fair sharing; per-scan accounts
internal/cache        bounded TTL cache with single-flight loading (shared discovery cache)
internal/store        in-memory Repository (swap for PostgreSQL later)
internal/discovery    Engine interface, Finding, State, Target/scope
  subdomains          passive hostname discovery; Source providers: crt.sh, Cert Spotter
  archive             archived URLs from the Wayback Machine CDX index
  dnsresolve          resolves hosts (injectable Resolver)
  httpprobe           checks https:// then http:// on resolved hosts
  sitemap             robots.txt Sitemap: lines and XML/text sitemaps
  htmlcrawl           crawls reachable hosts, each with its own budget
internal/results      Aggregator: merges findings into hosts → URLs, implements State
internal/normalize    URL and hostname canonicalization (deduplication keys)
internal/classify     page / api / asset / unknown, always with evidence
internal/fetch        outbound HTTP: timeouts, size caps, per-host rate limit,
                      global concurrency pool, per-scan request budget,
                      no auto-redirects, non-public address guard
internal/htmlmeta     <title> extraction
internal/config       environment configuration
```

### Scan modes

Every scan lists subdomains. The mode decides how routes are found and how
much the scanner contacts the site (`"mode"` in `POST /api/scans`, default
`SCAN_DEFAULT_MODE=light`):

| Mode | Contacts the site? | Routes come from |
| --- | --- | --- |
| `passive` | never | the web archive (historical; may no longer exist) |
| `light` | one probe plus robots.txt and sitemaps per live host | archive + sitemaps |
| `full` | also crawls pages, following links | archive + sitemaps + links |

Measured cold (nothing cached) with the deployment profile:

| Site | Mode | Time | Requests | Downloaded | Hosts (live) | URLs |
| --- | --- | --- | --- | --- | --- | --- |
| imanandhu.in | passive | 3.7 s | 4 | 0.05 MB | 15 (not checked) | 2 |
| imanandhu.in | light | 13.1 s | 35 | 0.16 MB | 15 (11) | 12 |
| imanandhu.in | full | 65.6 s* | 67 | 0.57 MB | 15 (11) | 160 |
| python.org | passive | 17.8 s | 6 | 0.11 MB | 48 (not checked) | 1,089 |
| python.org | light | 52.8 s | 186 | 5.3 MB | 74 (54) | 2,496 |
| python.org | full | 142.9 s | 4,677 | 28.0 MB | 78 (54) | 13,427 |

\* 59 s of it waiting for crt.sh; the scanner's own work took about 7 s.

Small sites are barely archived, so passive mode finds few routes for them;
large sites are well archived. Archived URLs are never requested, also not
in full mode: crawling them would mean requesting possibly dead URLs just to
verify them. They are shown as discovered, with when the archive first
captured them and the content type it recorded.

### Pipeline

The scan runs an ordered list of stages, defined in `pipeline()` in
[cmd/server/main.go](cmd/server/main.go). Each stage is one progress step;
a scan runs only the stages of its mode.

| Step | Modes | Engines | What happens |
| --- | --- | --- | --- |
| Validating target | all | (built in) | Input parsed and scope derived when the scan is created |
| Discovering subdomains | all | `subdomains` | Certificate Transparency via crt.sh and Cert Spotter, run concurrently and merged |
| Searching web archives | all | `archive` | One Wayback Machine CDX query for the domain and all subdomains: URLs archived with HTTP 200, first capture, content type |
| Resolving discovered hosts | all | `dns` | A/AAAA/CNAME lookup for every known host |
| Checking which hosts are live | light, full | `http` | `GET https://host/`, falling back to `http://host/`, on resolved public hosts |
| Reading robots.txt and sitemaps | light, full | `sitemap` | `Sitemap:` lines of robots.txt (never `Disallow`), else `/sitemap.xml`; sitemap indexes, `.xml.gz` and text sitemaps; listed URLs are not requested |
| Crawling reachable hosts | full | `html` | BFS crawl per reachable host, seeded from sitemaps |
| Checking hosts found in sitemaps / while crawling | light / full | `dns`, `http`, `sitemap` (+ `html`) | The same engines again, for hosts first seen in sitemaps, links or redirects |
| Finalizing results | all | (built in) | Aggregate and store |

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

### Resource management

Several users can scan large domains at the same time. The server bounds its
total work in three layers, so no scan (and no number of scans) can grow its
resource use without limit.

**1. Scheduler: how many scans run.** At most `MAX_CONCURRENT_SCANS` scans run
at once. Further scans wait in a FIFO queue of up to `SCAN_QUEUE_SIZE` (beyond
that, `POST /api/scans` returns `503`). A queued scan reports its
`queuePosition` and starts automatically when a running scan ends. Queued and
running scans can be cancelled. There is one goroutine per running scan and
none for queued scans.

**2. Shared pools: how much work all running scans do together.** Every
outbound operation takes a slot from a pool shared by all scans, and gives it
back as soon as that single operation ends:

| Pool | Size | Covers |
| --- | --- | --- |
| `http` | `GLOBAL_HTTP_CONCURRENCY` | every probe and crawl request, including reading its body |
| `dns` | `GLOBAL_DNS_CONCURRENCY` | each host's A/AAAA and CNAME lookups |
| `certificate-transparency` | 4 (fixed) | crt.sh / Cert Spotter queries, whose responses can be tens of MB |

Per-scan settings such as `SCAN_HOST_CONCURRENCY` and `SCAN_CONCURRENCY` only
limit how many operations one scan asks for at once; the pools decide how many
actually run. Slots are shared **fairly**: when a slot frees up and several
scans are waiting, it goes to the waiting scan that holds the fewest slots.
A large scan can use the whole pool while it is alone, but as soon as a small
scan needs capacity, freed slots flow to the small scan until they hold equal
shares. Rate-limit delays (`SCAN_REQUESTS_PER_SECOND` per host) are waited out
*before* taking a slot, so politeness pauses never occupy shared capacity.

**3. Per-scan budgets: how much one scan may do in total.** These are
safety limits for very large domains, not the intended coverage. Defaults
are generous; a scan that hits one reports it (see `limits` below) instead
of pretending to be complete.

| Budget | Setting | When reached |
| --- | --- | --- |
| duration | `SCAN_TIMEOUT` | the scan stops, keeps its results, `stopReason: "scan_timeout"` |
| requests | `SCAN_MAX_REQUESTS` | remaining probes/crawls are skipped (`request_budget_reached`) |
| hostnames recorded | `SCAN_MAX_DISCOVERED_HOSTS` | further names are counted, not listed (`host_budget_reached`) |
| hosts resolved / probed / crawled | `SCAN_MAX_RESOLVE_HOSTS`, `SCAN_MAX_PROBE_HOSTS`, `SCAN_MAX_HOSTS` | the rest are marked `skipped` (`host_budget_reached`) |
| requests per host | `SCAN_MAX_URLS` | that host's crawl stops (`crawl_limit_reached`) |
| URLs recorded | `SCAN_MAX_RECORDED_URLS_PER_HOST`, `SCAN_MAX_RECORDED_URLS` | further URLs are counted, not stored (`url_budget_reached`) |
| response size | `SCAN_MAX_BODY_BYTES` | the body is truncated |

**Backpressure.** Nothing queues without a bound: crawl frontiers are cut to
the host's remaining request budget (links beyond it are still recorded as
discovered, just not queued for fetching), worker goroutines are created
only after acquiring a per-scan concurrency slot, and results keep one entry
per normalized URL or host with metadata only (no response bodies).

**Cancellation and time limits.** Each scan runs under its own context,
cancelled by `POST /api/scans/{id}/cancel`, by `SCAN_TIMEOUT` or by server
shutdown. The context reaches pool waits, rate-limit waits, DNS lookups and
HTTP requests, so all of the scan's work stops within moments and its pool
slots are released immediately. The scan then saves the results it has.

**Shutdown.** On SIGINT/SIGTERM the server stops accepting scans, marks
queued scans `cancelled` (`stopReason: "server_shutdown"`), interrupts running
scans (which save partial results), waits up to 15 s for them, then stops
serving HTTP.

**Progress.** Status responses contain only fixed-size counters (see
`counts` below), never the discovered hosts or URLs, so polling stays cheap
for any scan size. URL counters are maintained incrementally.

**Retention.** Finished scans stay in memory until evicted, oldest first,
when there are more than `MAX_STORED_SCANS` of them or their results hold
more than `MAX_STORED_RESULT_URLS` URLs in total. Queued and running scans are
never evicted. The URL budget is the real memory bound: measured, a stored
result costs about 0.6 KB per URL (1 KB per URL while the scan runs), so a
scan at the 50,000-URL cap keeps about 30 MB.

**Bandwidth.** Assets referenced by pages (scripts, styles, images, fonts,
media, documents) are recorded, never requested. The scanning client reads
response bodies only for HTML; other responses (JSON, XML, feeds) are
classified from their status and headers, and their bodies are not
downloaded.

`GET /api/health` shows the scheduler and each pool's live use.

### Shared discovery cache

Scans reuse discovery work done by earlier or concurrent scans. The cache
holds reusable *pieces* of discovery, never whole scan results. Each scan
still builds its own result, combining cached and newly observed data.

| Layer | Key | Holds | TTL (default) |
| --- | --- | --- | --- |
| certificate | provider + domain | in-scope hostnames from one CT provider, or its failure | `CACHE_CERT_TTL` (6 h); rate limited 15 min; other failures 5 min |
| dns | hostname | addresses, CNAME, non-public flag, or NXDOMAIN | `CACHE_DNS_TTL` (5 min); NXDOMAIN 1 min; timeouts never |
| probe | hostname + target scope | reachable?, status, redirect, final URL, title, server | `CACHE_PROBE_TTL` (2 min), reachable or not |
| page | URL | status, content type, title, redirect, and every link/asset reference | `CACHE_PAGE_TTL` (15 min) |
| archive | domain | archived URLs with first capture and content type | `CACHE_ARCHIVE_TTL` (24 h); failures 5 min |
| sitemap | URL | a robots.txt's sitemap list or a sitemap's URLs | `CACHE_PAGE_TTL` (15 min) |

- **What is never cached:** response bodies, asset contents, and outcomes
  that belong to one scan rather than to the data (cancellation, a spent
  request budget). Transient network failures of pages and DNS timeouts are
  never cached either.
- **Why these TTLs:** certificates change slowly and Cert Spotter allows
  about 10 unauthenticated requests per hour, so 6 hours. DNS records
  commonly use a TTL of about 5 minutes; Go's resolver does not expose the
  real TTL, so a bounded one is used instead. Reachability changes fastest,
  so probes get the shortest TTL.
- **DNS caching is safe for SSRF protection:** the cache only affects what a
  scan reports. Every HTTP connection still resolves the name again and
  checks the actual address it connects to.
- **Cache hits skip the pools.** Each layer wraps exactly the step that costs
  network: a certificate hit contacts no provider, and a DNS, probe or page
  hit takes no pool slot and uses none of the scan's request budget. A miss
  goes through the Phase 1 pools like any request. There is no second
  concurrency system.
- **Single-flight.** When several scans miss the same key at once (for
  example two scans of the same domain started together), one of them does
  the lookup and the others wait for its answer. If that load fails for a
  reason specific to its scan, the waiters load for themselves.
- **Crawling from the cache covers the same pages.** Cached pages count
  against the host's page budget exactly as fetched pages do, so a warm crawl
  maps the same URLs; `crawl.fromCache` counts them separately from
  `crawl.requests`.
- **Provenance.** Observations reused from the cache carry `cachedAt` (when
  they were actually made): on DNS and probe results, on verified URLs, and
  as `fromCache` on hosts known only from cached data. `counts.cache`
  totals what was reused; everything else was newly observed.

**Memory.** `CACHE_MAX_MB` (64 MB) bounds the whole cache: 50% pages, 20%
archive listings, 10% sitemaps, 10% DNS, 5% probes and 5% certificates. Entries are charged their estimated
size, including the cache's own per-entry bookkeeping; measured real heap is
0.84-0.97x the estimate. Within a layer, the least recently used entries are
evicted first. One domain may use at most a quarter of a layer and evicts its
own oldest entries beyond that, so one enormous domain cannot push the others
out. A single entry larger than a quarter of its layer is not stored.
`CACHE_MAX_MB=0` disables caching.

### Scan coalescing

An equivalent scan is one with the same normalized start URL and options
(scans take no per-request options yet). `example.com` and
`https://example.com/` are equivalent; `www.example.com`,
`http://example.com` or `example.com/docs` are not, because they start
crawling elsewhere.

- A request for a scan equivalent to one that is **queued or running** joins
  it: it gets the same scan ID and its own `subscriptionId`, and adds no job,
  no worker and no queue slot (it works even when the queue is full). All
  subscribers read the same status and results. Finished scans are never
  joined; a new request starts a new scan, which reuses the cache.
- **Cancellation is per subscriber.** `POST /api/scans/{id}/cancel` with
  `{"subscriptionId": "..."}` releases that subscription only; the response
  has `"detached": true` while others remain. When the **last** subscription
  is released the scan itself is cancelled (removed from the queue, or
  interrupted with partial results kept), since nobody is waiting for it. A
  cancel without a subscription works only for an unshared scan and returns
  `409` for a shared one, so an older client cannot cancel other people's
  scan. Closing the browser does not release a subscription.

### Memory on a shared server

Web Scanner shares its VPS with other services (FileDrop), so its memory
is bounded by configuration rather than by what the machine has:

| Part | Bound (defaults) | Approximate memory |
| --- | --- | --- |
| discovery cache | `CACHE_MAX_MB=64` | ~64 MB |
| running scans | `MAX_CONCURRENT_SCANS=3` x `SCAN_MAX_RECORDED_URLS=50000` | up to ~50 MB each |
| stored results | `MAX_STORED_RESULT_URLS=500000` | up to ~300 MB |

These are the development defaults. For a small server use the deployment
profile below, which keeps the live heap around 100-165 MB.

With Go's garbage collector the process can briefly use up to about twice
its live heap. On a small VPS lower `MAX_STORED_RESULT_URLS` first, since
stored results are the largest part; setting the Go runtime's
`GOMEMLIMIT` (for example `GOMEMLIMIT=600MiB`) also makes the collector work
harder before the process grows.

### Deploying on a small shared server

[deploy/websitemapper.env](deploy/websitemapper.env) is a profile for a
2 vCPU / 2 GiB VPS that also runs other services, and
[deploy/websitemapper.service](deploy/websitemapper.service) is a systemd
unit with install steps. Protection works in two layers:

1. **The application's own limits** (the profile) keep normal use small.
2. **Kernel limits** (cgroup v2, via the unit) are a hard backstop: above
   `MemoryHigh=400M` this service is throttled and reclaimed, at
   `MemoryMax=512M` only this service is OOM-killed and restarted, and
   `CPUQuota=60%` / `CPUWeight=20` / `Nice=10` make other services win the
   CPU whenever the machine is busy. Go 1.23 does not read cgroup CPU limits,
   so the profile also sets `GOMAXPROCS=1`, and `GOMEMLIMIT=300MiB` makes the
   garbage collector work harder before the process grows.

**Bandwidth.** `GLOBAL_DOWNLOAD_KBPS` caps what all scans download together,
counted in bytes on the wire (the scanner requests gzip and decompresses
itself, so compressed pages count at their compressed size, which is what
the network carries and the provider bills). It is enforced as bodies are
read: the remote server is slowed by TCP flow control. A rate is also a
monthly ceiling: 512 KB/s is at most 1.3 TB in 30 days even if scans never
stopped. `SCAN_MAX_DOWNLOAD_MB` caps each scan. Certificate-log answers
count too. Responses to browsers are gzipped, and only the scan status
(about 2 KB) is polled while a scan runs.

Measured with this profile, running python.org and go.dev at the same time
and then imanandhu.in:

| | Measured | Limit |
| --- | --- | --- |
| peak heap | 101 MB | `GOMEMLIMIT` 300 MiB |
| peak memory from the OS | 139 MB | `MemoryHigh` 400 MB |
| CPU | 6.7% of one core (average) | `GOMAXPROCS=1`, `CPUQuota=60%` |
| download (on the wire) | 381 KB/s average, 561 KB/s peak over 10 s (one-second burst) | 512 KB/s |
| python.org results sent to a browser | 5.63 MB, 0.36 MB gzipped | |
| scan time | python.org 187 s, go.dev 175 s (in parallel), imanandhu.in 15 s | |

What sets scan time, in order:

1. **Politeness per host:** at most `SCAN_REQUESTS_PER_SECOND` (5) requests
   per second to one host, so a host crawled to `SCAN_MAX_URLS` (300) takes
   at least 60 s. Large sites have many such hosts; `SCAN_HOST_CONCURRENCY`
   crawls several at once. Lower `SCAN_MAX_URLS` for faster, shallower maps.
2. **The download cap:** sites that do not compress (go.dev served about
   48 MB) use it up while others share it.
3. **HTTP slots:** each slot is held for a response's full duration (often
   0.5 s or more), so `GLOBAL_HTTP_CONCURRENCY` bounds requests per second.
   Slots are cheap for the server (memory is bounded by
   `SCAN_MAX_BODY_BYTES` per slot, bandwidth by the cap). An earlier profile
   with 12 slots made python.org take 325 s instead of 187 s.

`/api/health` shows live memory (`process`), bandwidth used and per-pool use,
for monitoring.

The API listens on all interfaces on `SERVER_PORT`; on a public server keep
it behind the Next.js client or a reverse proxy and block the port in the
firewall. If the Next.js client runs on the same machine, give it its own
limits too (for example `NODE_OPTIONS=--max-old-space-size=256` and a
similar systemd unit).

### Design notes

Reviewed and deliberately left as they are:

- **No separate HTML-parsing pool.** Parsing runs at about 53 MB/s per core
  (1.9 ms for a 100 KB page). Parsing can't outrun downloading, and the
  global HTTP pool bounds downloads, so it bounds parsing CPU as well: even a
  full 1 Gbit/s link needs about 2.4 cores. Bodies are dropped as soon as
  they are parsed.
- **Progress ticks scan the hosts.** Host counters are recomputed each second
  (236 µs for 10,000 hosts, no allocations); URL counters are incremental.
  Making host counters incremental would add bookkeeping for no measurable
  gain.
- **The per-host rate stays at 5 requests/second, and crawl parallelism at
  8 hosts per scan.** One large host occupies one crawl slot while the
  others keep flowing (tested). One scan's crawl ceiling is 8 hosts x 5/s =
  40 requests/s. Measured on python.org (46 hosts, ~2,130 requests), the
  global pool of 32 slots sustained about 47 requests/s, because real
  responses hold a slot for their full duration. Raising host parallelism
  to 32 made 91% of requests queue at the pool (average 0.5 s) and gave no
  reliable speed-up (crawl stage 45-93 s vs 63-78 s at 8). In a synthetic
  test with fast hosts, 32 was 2.5x faster, so raise `SCAN_HOST_CONCURRENCY`
  together with `GLOBAL_HTTP_CONCURRENCY` when targets respond quickly.
  One remaining inefficiency: a request that had to wait for a pool slot
  takes a fresh per-host rate slot, which keeps politeness exact but can
  slow a heavily contended host.
- **One follow-up round.** Hosts first found while crawling are resolved,
  probed and crawled once more. Hosts first found in that last round are
  listed but not processed, and the scan says so
  (`discovery_round_limit`). More rounds would be safe (every budget counts
  hosts across rounds) but real scans found almost no hosts in the
  follow-up round, so they are not worth the extra time yet.
- **Certificate providers are isolated.** Each has its own deadline
  (`SCAN_CT_TIMEOUT`, retries included); hosts from a provider that answered
  are always kept, and a failed provider is recorded as a partial error.
  Answers (and brief failures) are cached across scans; see Shared
  discovery cache.
- **Shutdown** is covered by unit tests. A real SIGINT/SIGTERM end-to-end test
  is still to be run on Linux or macOS, since Windows cannot deliver the
  signal to a background process.

### Host budgets

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
{
  "status": "ok",
  "scheduler": {
    "running": 3, "queued": 2, "maxRunning": 3, "queueSize": 100,
    "pools": [
      { "name": "http", "capacity": 32, "inUse": 32, "waiting": 41, "peak": 32, "acquired": 18231, "waitedMs": 912004 },
      { "name": "dns", "capacity": 16, "inUse": 0, "waiting": 0, "peak": 16, "acquired": 7390, "waitedMs": 20411 },
      { "name": "certificate-transparency", "capacity": 4, "inUse": 1, "waiting": 0, "peak": 4, "acquired": 12, "waitedMs": 0 }
    ]
  }
}
```

`peak` is the highest number of slots ever in use at once; it never exceeds
`capacity`. The scheduler also reports `coalescedRequests` and, per cache
layer, `hits`, `misses`, `coalesced` (misses that waited for another
caller's lookup), `evictions`, `expired`, `rejected`, `entries` and
`costBytes` / `maxCostBytes`.

### `POST /api/scans`

Request:

```json
{ "target": "example.com", "mode": "light" }
```

`target` can be a bare domain or a URL such as `https://www.example.com/docs`.
`mode` is `passive`, `light` or `full` (see Scan modes); it may be omitted.
Scans in different modes never coalesce.

Response `202 Accepted` (with a `Location` header) contains the scan status
object described below. It returns immediately: `"status": "running"` if
capacity was free, otherwise `"status": "queued"` with a `queuePosition`.
It also contains `subscriptionId` (keep it to cancel), and
`"coalesced": true` when the request joined an equivalent scan that was
already queued or running (see Scan coalescing).

Errors: `400` for an invalid target or body, and `503` with `Retry-After`
when the queue is full or the server is shutting down.

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
  "phase": "resolving_hosts",
  "progress": { "total": 15, "completed": 11, "pending": 4 },
  "limits": [],
  "resources": {
    "requests": 3, "maxRequests": 50000,
    "pools": { "dns": { "operations": 11, "delayed": 2, "avgWaitMs": 40 } }
  },
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
    "hostsUnresolved": 0, "hostsResolvePending": 4, "hostsProbed": 0,
    "hostsUnreachable": 0, "hostsProbePending": 11, "hostsCrawlPending": 0,
    "urls": 0, "pages": 0, "apis": 0, "assets": 0, "javascript": 0,
    "urlsFetched": 0, "urlsFailed": 0,
    "limits": {
      "hostsOmitted": 0, "resolveSkipped": 0, "probeSkipped": 0,
      "crawlSkipped": 0, "crawlLimited": 0, "urlsOmitted": 0
    }
  },
  "errors": []
}
```

- `status`: `queued` | `running` | `completed` | `failed` | `cancelled`
- `subscribers`: how many requesters share the scan while it is active.
- `counts.cache`: observations reused from the shared cache (`hosts` known
  only from cached data, `dns`, `probes`, verified `pages`).
- `queuePosition`: 1-based, only while `queued`.
- `phase`: `queued` | `discovering_subdomains` | `resolving_hosts` |
  `probing_hosts` | `crawling_hosts` | `finalizing` | `done`.
- `progress`: the current phase measured in hosts (resolving, probing and
  crawling only), e.g. "Probing 6,921 / 7,200".
- `stopReason`: set when the scan ended early: `scan_timeout` (status
  `completed`, results partial), `cancelled` or `server_shutdown` (status
  `cancelled`).
- `limits`: notices about budgets that shaped the result, each
  `{ "code", "message" }`. Codes: `scan_timeout`, `request_budget_reached`,
  `host_budget_reached`, `crawl_limit_reached`, `url_budget_reached`,
  `global_resource_wait` (operations waited a noticeable time for shared
  capacity: slower, but nothing was dropped), `discovery_round_limit` (hosts
  found in the last crawl round were listed but not checked).
- `resources`: requests used against the budget, and per pool how many of
  this scan's operations had to wait and for how long on average.
- `counts`: fixed-size aggregate counters. `*Pending` hosts are waiting for
  that stage; `counts.limits` counts work cut short by a budget.
- `steps[].status`: `pending` | `running` | `done` | `failed` | `skipped` |
  `stopped` (interrupted by cancellation or the time limit)
- `errors[].partial`: `true` when the step still produced results, e.g. one
  certificate provider failed and the other answered.
- `errors`: engine failures, e.g.
  `{ "stage": "subdomains", "engine": "subdomains", "message": "crt.sh: …; certspotter: …" }`.
  A `completed` scan can still have errors; its results are then partial.
- `error`: set only when the whole scan failed.
- `finishedAt` and `durationMs` are set once the scan finishes.

`404` if the scan does not exist. Scans live in memory and are lost on restart.

### `POST /api/scans/{id}/cancel`

Body (optional): `{"subscriptionId": "..."}` from the create response.

Releases the subscription and returns `202` with the scan status. If other
requesters still share the scan, the response has `"detached": true` and the
scan continues. Otherwise the scan is cancelled: a queued scan is
`cancelled` immediately and never runs; a running scan stops within moments,
releases its share of the server, and saves the results collected so far
(its status becomes `cancelled` shortly after the response).

`409` if the scan already finished, if it is shared and no `subscriptionId`
was given, or if the subscription is unknown or already released. `404` if
the scan does not exist.

### `GET /api/scans/{id}/results`

Available once the scan is `completed`, `failed` or `cancelled` (after it
started running). Returns `409` with `{"error": "...", "status": "running"}`
before that, and for scans cancelled while still queued. The result carries
the same `stopReason` and `limits` as the status, plus `hostsOmitted`.

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
| `crawl` | `requests`, `limitReached`, or `skipped` with a reason |
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

## Notes on data sources

- **crt.sh** returns the full certificate history, which includes names that
  no longer resolve. It is slow and often returns 502/503 under load, so it
  is retried twice.
- **Cert Spotter** returns only current certificates. Unauthenticated use is
  limited to about 10 requests per hour per IP; each scan uses up to 3.
- Neither source is a complete DNS inventory. Hosts that never had their own
  certificate (for example ones covered only by a wildcard) will not appear
  unless they are linked from a crawled page.
