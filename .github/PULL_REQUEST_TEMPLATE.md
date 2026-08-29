## What & why

<!-- What does this change, and why? Link any related issue. -->

## Checklist

- [ ] `go mod tidy -diff` and `go fix -diff ./...` print nothing
- [ ] `go vet ./...` passes
- [ ] `golangci-lint run` passes
- [ ] `go test -race ./...` passes
- [ ] New/changed metrics are documented in the README tables
- [ ] Label cardinality is bounded (respects `TOPN` / `FANOUT_LIMIT` where relevant)
- [ ] The Immich client remains read-only
