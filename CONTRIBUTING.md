# Contributing to immich-exporter

Thanks for taking the time to contribute! This is a small, focused project — a read-only Prometheus exporter for Immich library content. Bug reports, metric suggestions, dashboard improvements and docs fixes are all welcome.

## Development setup

You need Go (see the version in [`go.mod`](go.mod)) and, for linting, [golangci-lint](https://golangci-lint.run).

```sh
git clone https://github.com/moond4rk/immich-exporter
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

## Dashboard changes

Prometheus and VictoriaMetrics evaluate some queries differently (VictoriaMetrics matches a label-less `sum(...)` like a scalar and drops `NaN` results, for example), so `grafana/dashboard.json` is checked against both:

- `go test ./grafana`, part of the normal test run, checks that every queried metric is in `internal/exporter/testdata/metrics.golden` and that range windows cover four scrape intervals of [`deploy/prometheus.yml`](deploy/prometheus.yml).
- The **Dashboard** workflow runs every panel query on Prometheus and VictoriaMetrics instances that scrape the golden metrics, and fails if either backend errors, returns no data, or returns different series. To run it locally:

```sh
docker compose -f grafana/testdata/compose.yml up -d --wait
DASHBOARD_PROMETHEUS_URL=http://127.0.0.1:19090 DASHBOARD_VICTORIAMETRICS_URL=http://127.0.0.1:18428 \
  go test ./grafana -count=1
docker compose -f grafana/testdata/compose.yml down
```

A query on a metric the poll mock doesn't produce yet needs it in `internal/exporter/poll_test.go` first, then `UPDATE_GOLDEN=1 go test ./internal/exporter -run TestGoldenMetrics`.

The `datasource` variable's current value stays `${DS_PROMETHEUS}` with text `Prometheus`, so an import uses the datasource picked in its dialog and file provisioning falls back to the one named Prometheus. A dashboard re-exported from Grafana loses this, and `go test ./grafana` catches it.

After a visible dashboard change, refresh the README screenshots with `grafana/demo/screenshots.sh` (needs Docker, Go, jq and ImageMagick). It renders the dashboard over a synthetic library from `internal/exporter/demo_test.go`, so no real data is involved, and `KEEP=1` leaves Grafana running on http://127.0.0.1:13001 to look around. A new metric needs a demo value too, or `TestDemoCoversEveryMetric` fails.

## Commit messages

Imperative mood, concise subject (≤ 72 chars). Explain the _why_ in the body when it isn't obvious.

## License

By contributing, you agree that your contributions will be licensed under the [Apache-2.0 License](LICENSE).
