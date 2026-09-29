# Website Mapper

> Enter a domain. Understand what exists on the public internet.

Website Mapper builds a map of what is publicly discoverable about a website:
its hosts, pages, API-like endpoints, assets and technologies. For each result
it shows how the result was found. The scanner is passive and low-impact. It
is not a vulnerability scanner.

```text
server/   Go API, scan orchestration and discovery engines  → server/README.md
client/   Next.js web UI                                     → client/README.md
```

## Run locally

You need Go 1.23+ and Node.js 20.9+. Use two terminals:

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
inspect. You must confirm that permission before a scan starts; the API
enforces it too.

Before scanning sites you don't control, set `BOT_INFO_URL` (the public URL
of the client's `/bot` page) and `BOT_CONTACT` for the server. Site owners
can then identify the crawler and reach you. The crawler respects
robots.txt by default. See [server/README.md](server/README.md#responsible-use).

## Test and build

```sh
cd server && gofmt -l . && go vet ./... && go test ./... && go build -o bin/server ./cmd/server
cd client && npm run typecheck && npm run lint && npm test && npm run build
```
