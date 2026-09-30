## What and why

<!-- What does this change, and why is it needed? Link the issue if there is one. -->

## How it was tested

<!-- Tests added or updated, and anything you checked by hand. -->

## Checklist

- [ ] `gofmt -l .`, `go vet ./...` and `go test ./...` pass in `server/`
- [ ] `npm run typecheck`, `npm run lint`, `npm test` and `npm run build` pass in `client/`
- [ ] Docs updated if behaviour, the API or settings changed
- [ ] Keeps the scanner passive (GET only, in scope, rate-limited; see CONTRIBUTING.md)
