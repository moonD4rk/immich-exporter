# Contributing to immich-exporter

Thanks for taking the time to contribute! This is a small, focused project — a read-only Prometheus exporter for Immich library content. Bug reports, metric suggestions, dashboard improvements and docs fixes are all welcome.

## Development setup

You need Go (see the version in [`go.mod`](go.mod)) and, for linting, [golangci-lint](https://golangci-lint.run).

```sh
git clone https://github.com/moonD4rk/immich-exporter
cd immich-exporter
go build ./cmd/immich-exporter
```

Run it against a real Immich instance:

```sh
IMMICH_HOST=immich.example.com IMMICH_API_TOKEN=<admin-key> \
  go run ./cmd/immich-exporter
# metrics on http://localhost:8000/metrics
```

## Before you open a pull request

Please make sure the same checks CI runs pass locally:

```sh
gofmt -l .          # must print nothing
go vet ./...
golangci-lint run   # config in .golangci.yml
go test -race ./...
```

- Keep code self-documenting; comments explain _why_, not _what_.
- **New metrics:** add the descriptor in `internal/exporter/collector.go`, emit it, register it in `allDescs`, document it in the README metrics tables, and prefer gauges (no `_total` suffix unless the value only ever increases).
- **Watch label cardinality.** Breakdowns that can explode must respect `TOPN` and `FANOUT_LIMIT` like the existing camera/person fan-outs.
- **The Immich client stays read-only** (`GET`, plus `POST /search/statistics` which is a read-only count query). The exporter must never mutate a library.
- Admin-only or version-dependent endpoints should be collected through the `soft(...)` path in `poll.go` so the exporter degrades gracefully on non-admin keys and older Immich versions.

## Commit messages

Imperative mood, concise subject (≤ 72 chars). Explain the _why_ in the body when it isn't obvious.

## License

By contributing, you agree that your contributions will be licensed under the [Apache-2.0 License](LICENSE).
