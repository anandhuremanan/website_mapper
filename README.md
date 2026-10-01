# Web Scanner

> Enter a domain. Understand what exists on the public internet.

Web Scanner lists what is publicly discoverable about a website: its
**subdomains** and the **routes** on each of them (pages, API-like endpoints
and assets), with **how every result was found**. It answers the question
"I own this domain; what can someone discover about it from the outside?"

It is a passive discovery tool, not a vulnerability scanner: it reads public
sources, sends ordinary `GET` requests at a polite rate, and never submits
forms, guesses paths or brute-forces names.

[![CI](https://github.com/anandhuremanan/website_mapper/actions/workflows/ci.yml/badge.svg)](https://github.com/anandhuremanan/website_mapper/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

## Features

- **Subdomains from certificate logs and public subdomain databases**,
  checked in DNS; names that no longer resolve stay visible.
- **Routes from several sources:** a public web archive (without contacting
  the site), robots.txt and sitemaps, and optionally a crawl of each live
  host.
- **Provenance for everything:** each host and URL says where it came from,
  whether the scanner requested it, and why it is classified as a page, API
  or asset.
- **Three scan depths:** passive, standard and full crawl (below).
- **Built for small servers:** global limits on concurrent work, fair sharing
  between users, a bandwidth cap, per-scan budgets, a bounded shared cache
  and a queue. Identical scans started at the same time are merged into one.
  Limits that shaped a result are always reported.

## Scan modes

| Mode | Contacts the site? | Routes come from |
| --- | --- | --- |
| Passive | never | the web archive (may be out of date) |
| Standard *(default)* | one check per live host, plus its robots.txt and sitemaps | archive + sitemaps |
| Full crawl | also follows links on every live host | archive + sitemaps + links |

How long a scan takes depends mostly on how much the web archive holds for
the domain: the listing is read at no more than 20 requests a minute, about
25,000 URLs each, in the background while the rest of the scan runs. A
passive scan of python.org limited to 200,000 archived URLs took 80 s.
Results can be browsed while a scan runs. See
[server/README.md](server/README.md#scan-modes).

## Quick start

You need **Go 1.26+** and **Node.js 20.9+**. Use two terminals:

```sh
# 1. API on :8080
cd server
go run ./cmd/server

# 2. UI on :3000 (proxies /api to :8080)
cd client
npm install
npm run dev
```

Open http://localhost:3000 and enter a domain you own or have permission to
inspect. No configuration is needed; every setting has a default.

## Test and build

```sh
cd server && gofmt -l . && go vet ./... && go test ./... && go build -o bin/server ./cmd/server
cd client && npm run typecheck && npm run lint && npm test && npm run build
```

## Deploying

The API is a single static Go binary. It keeps scans and results in its own
data directory (one SQLite file per scan), so there is no database server to
run.

- [server/deploy/websitemapper.env](server/deploy/websitemapper.env) is a
  settings profile for a small VPS (2 vCPU / 2 GiB) shared with other
  services.
- [server/deploy/websitemapper.service](server/deploy/websitemapper.service)
  is a systemd unit with hard memory and CPU limits, so the scanner can never
  starve other services.
- [.github/workflows/deploy.yml](.github/workflows/deploy.yml) tests, builds
  and deploys the API over SSH on pushes to `main`. It needs the repository
  secrets `VPS_HOST`, `VPS_SSH_PORT`, `VPS_USER` and `VPS_DEPLOY_KEY`.
- The web client (`client/`) is built and deployed by Vercel.

Keep the API port closed to the internet and serve it through the client or
a reverse proxy. Finished scans survive a restart and are removed after a
week or when the disk budget is reached; the discovery cache is in memory
and starts empty. Memory, CPU, disk and bandwidth behaviour are described in
[server/README.md](server/README.md#deploying-on-a-small-shared-server).

## Documentation

| Document | For |
| --- | --- |
| [ARCHITECTURE.md](ARCHITECTURE.md) | how it works and why, for contributors |
| [server/README.md](server/README.md) | the HTTP API, every setting, resource limits, caching |
| [client/README.md](client/README.md) | the web UI |
| [CONTRIBUTING.md](CONTRIBUTING.md) | setup, checks and how to contribute |
| [SECURITY.md](SECURITY.md) | reporting vulnerabilities |
| [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) | community guidelines |

## Responsible use

Only scan domains you own or have permission to inspect. Passive mode never
contacts the site; the other modes make a limited number of ordinary
requests, identified by the configured `User-Agent`, at no more than 5
requests per second per host by default.

## License

[MIT](LICENSE) © [Anandhu Remanan](https://imanandhu.in)
