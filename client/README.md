# Web Scanner — client

Next.js (App Router, TypeScript, Tailwind CSS) web UI for Web Scanner.

The browser only talks to Next.js. Requests to `/api/*` are proxied to the Go
server by [src/proxy.ts](src/proxy.ts), so no CORS setup is needed. Set
`API_URL` if the server is not on `http://localhost:8080`, and
`API_PROXY_SECRET` if the server was given one: the proxy then adds it, and
the visitor's address, to every request it forwards.

## Commands

```sh
npm install
npm run dev        # http://localhost:3000 (start the Go server too)
npm test           # unit tests (Vitest)
npm run typecheck
npm run lint
npm run build && npm start
```

## Visitor verification

Optional and off by default. Setting `NEXT_PUBLIC_TURNSTILE_SITE_KEY` shows
Cloudflare Turnstile's check on the form and on "Scan again"
([src/lib/verification.ts](src/lib/verification.ts)). It runs in the
background and appears only if Cloudflare wants the visitor to tick a box.
The token it produces is sent with the request to start a scan; the server
must be given the matching secret key (`TURNSTILE_SECRET_KEY`), or the token
is ignored.

## Analytics

Usage analytics are optional and off by default. Setting
`NEXT_PUBLIC_POSTHOG_KEY` (and `NEXT_PUBLIC_POSTHOG_HOST` for the project's
region) sends a small set of events to PostHog: a scan was started,
finished or cancelled, a result tab was opened, the CSV was downloaded. It
runs without cookies or browser storage, without automatic click capture
and without session recording, and never sends the scanned domain, hosts or
URLs. The events and their properties are listed in
[src/lib/analytics.ts](src/lib/analytics.ts).

## Structure

```text
src/app/page.tsx                 home: enter a domain
src/app/scans/[id]/page.tsx      progress while running, results when finished
src/components/scan-form.tsx     target and scan mode
src/components/scan-progress.tsx
src/components/site-header.tsx   top bar (hidden on the home page)
src/components/site-footer.tsx   credits, source and license links
src/components/results/          overview, hosts (DNS/HTTP/crawl + routes per host), map, URL lists
src/lib/api.ts, types.ts         typed API client mirroring server/README.md
src/lib/use-scan.ts              polls scan status, then loads the result summary
src/lib/use-pages.ts             loads hosts, URLs and tree levels a page at a time
```
