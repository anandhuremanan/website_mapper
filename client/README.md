# Web Scanner — client

Next.js (App Router, TypeScript, Tailwind CSS) web UI for Web Scanner.

The browser only talks to Next.js. Requests to `/api/*` are proxied to the Go
server through a rewrite in [next.config.ts](next.config.ts), so no CORS setup
is needed. Set `API_URL` if the server is not on `http://localhost:8080`.

## Commands

```sh
npm install
npm run dev        # http://localhost:3000 (start the Go server too)
npm test           # unit tests (Vitest)
npm run typecheck
npm run lint
npm run build && npm start
```

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
