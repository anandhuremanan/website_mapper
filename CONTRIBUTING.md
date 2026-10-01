# Contributing to Web Scanner

Thanks for your interest! Bug reports, fixes, documentation and new
discovery sources are all welcome.

Before writing code, skim [ARCHITECTURE.md](ARCHITECTURE.md). It explains how
a scan works, why the resource limits exist, and where things go. Its
[known rough edges](ARCHITECTURE.md#known-rough-edges) list is a good place
to find a first issue.

## Ground rules

Web Scanner is a **discovery** tool, not a security scanner. Contributions
must keep it polite and passive:

- only ordinary `GET` requests to in-scope hosts, at a limited rate;
- no form submission, path guessing, wordlists, brute forcing or port
  scanning;
- no requests to private or reserved addresses;
- every result keeps its provenance, and limits that shaped a result are
  reported.

Features that turn the tool into an attack or vulnerability scanner will not
be merged. See [Safety rules](ARCHITECTURE.md#safety-rules).

## Development setup

You need **Go 1.26+** and **Node.js 20.9+**.

```sh
git clone https://github.com/anandhuremanan/website_mapper.git
cd website_mapper

# Terminal 1: API on :8080
cd server
go run ./cmd/server

# Terminal 2: UI on :3000 (proxies /api to :8080)
cd client
npm install
npm run dev
```

Open http://localhost:3000. Only scan domains you own or have permission to
inspect, and prefer the passive mode while developing.

No configuration is needed for development; every setting has a default.
See [server/.env.example](server/.env.example) for all of them.

## Checks

Run these before opening a pull request. GitHub Actions runs the server
checks; the client is built and deployed by Vercel.

```sh
cd server
gofmt -l .          # must print nothing
go vet ./...
go test ./...
go test -race -short ./...   # needs cgo (Linux/macOS, or a C compiler on Windows)

cd ../client
npm run typecheck
npm run lint
npm test
npm run build
```

## Writing tests

- Tests must not touch the real internet. Engines take small interfaces
  (`Fetcher`, `Resolver`, `Source`); use in-memory fakes or `httptest`
  servers. Look at the existing `*_test.go` files for patterns.
- Prefer injected clocks and channels over `time.Sleep`. If a test must
  measure timing, use generous bounds and skip it under `-short` when it is
  slow.
- Test fakes used from several goroutines must not share mutable state (for
  example return a copy of a canned response, not the same pointer).
- Tests that need a result store use `storetest.New(t, store.Options{})`,
  which opens one in a temporary directory and closes it when the test ends.
- Client component tests use React Testing Library with jsdom: put
  `// @vitest-environment jsdom` at the top of `*.test.tsx` files.

## Code style

- Go: `gofmt`, standard library first, small packages with a package comment
  explaining what the package is for. Comment the *why*, not the *what*.
- TypeScript: the existing ESLint config; keep components small and data
  logic in `src/lib`.
- Keep the server's JSON and `client/src/lib/types.ts` in sync by hand when
  you change the API, and update [server/README.md](server/README.md#api).
- New settings go in `server/internal/config/config.go` and
  `server/.env.example` (and the deploy profile if they matter for small
  servers).

## Common changes

- **A new discovery engine or data source:** see
  [Extending the scanner](ARCHITECTURE.md#extending-the-scanner).
- **A UI change:** components live in `client/src/components`; add or update
  a component test when behaviour changes.
- **Docs:** fixes to any `.md` file are welcome, especially where the docs
  and the code disagree.

## Pull requests

1. Open an issue first for large changes, so we can agree on the approach.
2. Keep pull requests focused: one change per PR.
3. Describe what changed and why, and how you tested it.
4. Make sure the checks above pass.

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE).

## Reporting security issues

Please do not open public issues for vulnerabilities. See
[SECURITY.md](SECURITY.md).
