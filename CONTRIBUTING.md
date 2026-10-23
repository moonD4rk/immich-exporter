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
- **Watch label cardinality.** Breakdowns that can explode must respect `TOPN`, `FANOUT_LIMIT` or `GEO_CITY_LIMIT` like the existing camera, person and city breakdowns.
- **The Immich client stays read-only** (`GET`, plus `POST /search/statistics` which is a read-only count query). The exporter must never mutate a library.
- **Collectors** run through `r.soft(...)` or `r.hard(...)` in `poll.go`, which records `immich_exporter_collector_success` and its duration under the collector's name. Admin-only or version-dependent endpoints use `soft` so the exporter degrades gracefully on non-admin keys and older Immich versions; a 403 or 404 never fails the poll, even for a `hard` collector. Anything that pages through the whole library or fans out per value belongs in `collectBreakdowns`, which refreshes on `BREAKDOWN_INTERVAL`.
- **`StatisticsSearchDto.filter`** only exists from Immich 3.2, and older servers silently ignore unknown fields, so a filter body sent to them counts every asset. Gate filter queries on `searchFilter(version)`.
- **Alert rules** live in [`deploy/alerts.yml`](deploy/alerts.yml). `go test ./grafana` checks that they only use exported metrics, CI runs `promtool check rules` on them, and the Dashboard workflow evaluates each expression on Prometheus and VictoriaMetrics.

## Dashboard changes

`grafana/dashboard.json` is generated: the dashboard is defined in [`grafana/dashboard.go`](grafana/dashboard.go) from the panel kinds in [`grafana/design.go`](grafana/design.go), which also hold its visual rules (blue for volume, purple for videos, neutral text for counts, green, orange and red only for health). Edit the Go, then run `go generate ./grafana`; `go test ./grafana` fails while the two disagree, so a hand edit to the JSON or an export from Grafana doesn't survive. Grafana's editor is still the quickest place to try out a panel, then port what works to the Go.

Prometheus and VictoriaMetrics evaluate some queries differently (VictoriaMetrics matches a label-less `sum(...)` like a scalar and drops `NaN` results, for example), so the dashboard is checked against both:

- `go test ./grafana`, part of the normal test run, checks that every queried metric is in `internal/exporter/testdata/metrics.golden` and that range windows cover four scrape intervals of [`deploy/prometheus.yml`](deploy/prometheus.yml), counting `$__interval` at the panel's minimum interval.
- The **Dashboard** workflow runs every panel query on Prometheus and VictoriaMetrics instances that scrape the golden metrics, and fails if either backend errors, returns no data, or returns different series. To run it locally:

```sh
docker compose -f grafana/testdata/compose.yml up -d --wait
DASHBOARD_PROMETHEUS_URL=http://127.0.0.1:19090 DASHBOARD_VICTORIAMETRICS_URL=http://127.0.0.1:18428 \
  go test ./grafana -count=1
docker compose -f grafana/testdata/compose.yml down
```

A query on a metric the poll mock doesn't produce yet needs it in `internal/exporter/poll_test.go` first, then `UPDATE_GOLDEN=1 go test ./internal/exporter -run TestGoldenMetrics`.

The `datasource` variable's current value is `${DS_PROMETHEUS}` with text `Prometheus`, so an import uses the datasource picked in its dialog and file provisioning falls back to the one named Prometheus; `go test ./grafana` checks that the input stays referenced.

After a visible dashboard change, refresh the README screenshots with `grafana/demo/screenshots.sh` (needs Docker, Go, jq and ImageMagick). It renders the dashboard in the dark and light themes over a synthetic library from `internal/exporter/demo_test.go`, so no real data is involved, and `KEEP=1` leaves Grafana running on http://127.0.0.1:13001 to look around. A new metric needs a demo value too, or `TestDemoCoversEveryMetric` fails.

## Commit messages

Imperative mood, concise subject (≤ 72 chars). Explain the _why_ in the body when it isn't obvious.

## License

By contributing, you agree that your contributions will be licensed under the [Apache-2.0 License](LICENSE).
