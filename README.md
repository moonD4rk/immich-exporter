# immich-exporter

[![Go CI](https://github.com/moond4rk/immich-exporter/actions/workflows/ci.yml/badge.svg)](https://github.com/moond4rk/immich-exporter/actions/workflows/ci.yml) [![codecov](https://codecov.io/gh/moond4rk/immich-exporter/branch/main/graph/badge.svg)](https://codecov.io/gh/moond4rk/immich-exporter) [![Go Reference](https://pkg.go.dev/badge/github.com/moond4rk/immich-exporter.svg)](https://pkg.go.dev/github.com/moond4rk/immich-exporter) [![Go Report Card](https://goreportcard.com/badge/github.com/moond4rk/immich-exporter)](https://goreportcard.com/report/github.com/moond4rk/immich-exporter) [![License](https://img.shields.io/github/license/moond4rk/immich-exporter)](https://github.com/moond4rk/immich-exporter/blob/main/LICENSE)

A comprehensive Prometheus exporter for **Immich photo-library content** — ~90 metrics covering assets, cameras & EXIF, geography, people & faces, per-user storage & quota, albums & sharing, jobs, and server health. Ships with a large Grafana dashboard (world map included).

It is **complementary to Immich's built-in OpenTelemetry**: Immich's own telemetry exposes _performance_, never library _content_. All data is read from the public Immich REST API with an API key — the database is never touched.

## Features

- **~90 content metrics** Immich's telemetry doesn't expose — assets by type/year/rating/state, cameras & lenses, geo distribution by country, people & faces, per-user storage & quota, albums & shared links, tags, memories, duplicates, stacks, external libraries, and every job-queue depth.
- **Two-tier polling** with bounded-concurrency fan-out. An atomic snapshot is emitted fresh on every scrape, so vanished label combinations never linger as stale series.
- **Admin auto-detection** — full coverage with an admin key, graceful degradation with a non-admin one.
- **Single ~13 MB static binary**, distroless `nonroot` image; exports its own `go_*` / `process_*` runtime metrics too.
- **Batteries included** — Grafana dashboard (~100 panels), Docker Compose stack, and Kubernetes manifests.

## Quick start

### Docker

```sh
docker run -d --name immich-exporter -p 8000:8000 \
  -e IMMICH_HOST=immich-server -e IMMICH_API_TOKEN=<your-key> \
  ghcr.io/moond4rk/immich-exporter:latest
```

Metrics are served at `http://localhost:8000/metrics`. Use an **admin** API key (Account Settings → API Keys) for full coverage.

### Binary

Download from the [releases page](https://github.com/moond4rk/immich-exporter/releases), or install with Go:

```sh
go install github.com/moond4rk/immich-exporter/cmd/immich-exporter@latest
IMMICH_HOST=immich-server IMMICH_API_TOKEN=<your-key> immich-exporter
```

### Full monitoring stack

[`deploy/docker-compose.yml`](deploy/docker-compose.yml) brings up the exporter + Prometheus + Grafana with the dashboard and datasource auto-provisioned:

```sh
cp deploy/.env.example deploy/.env   # set IMMICH_API_TOKEN
docker compose -f deploy/docker-compose.yml up -d
# Grafana on http://localhost:3000 → "Immich Library" dashboard
```

See [`deploy/`](deploy/) for Kubernetes (Deployment + Service + ServiceMonitor) and a Prometheus scrape example.

## Configuration

Configuration is layered — each source overrides the one before it:

**built-in defaults → YAML file (`--config.file`) → environment variables → command-line flags**

Keep everything in one YAML file and still override a single value with an env var or flag at runtime. Run `immich-exporter --help` for the full list (each flag shows its `$ENV_VAR`).

### YAML config file

Point the exporter at a file with `--config.file=config.yml` (or `IMMICH_CONFIG_FILE`). Every key is optional; omitted keys keep their default, and unknown keys are rejected so typos fail loudly. Full template: [`deploy/config.example.yml`](deploy/config.example.yml).

```yaml
immich:
  base_url: http://immich-server:2283/api
web:
  listen_address: ":8000"
scrape: { interval: 2m, breakdown_interval: 15m, request_timeout: 20s }
collect: { camera: true, geo: true, ratings: true, people: false, heavy: true }
limits: { topn: 25, fanout_limit: 200, fanout_concurrency: 8 }
```

### Flags & environment variables

| Flag | Env var | Default | Description |
| --- | --- | --- | --- |
| `--immich.base-url` | `IMMICH_BASE_URL` | — | Full API base URL incl. `/api`; overrides host/port |
| `--immich.host` / `--immich.port` | `IMMICH_HOST` / `IMMICH_PORT` | `localhost` / `2283` | Immich location (API served under `/api`) |
| `--web.listen-address` | `EXPORTER_PORT` | `:8000` | Address/port to listen on |
| `--web.telemetry-path` | — | `/metrics` | Path to expose metrics under |
| `--scrape-interval` | `SCRAPE_INTERVAL` | `2m` | Cheap-metric poll interval |
| `--breakdown-interval` | `BREAKDOWN_INTERVAL` | `15m` | Expensive fan-out refresh interval |
| `--collect.people` | `COLLECT_PEOPLE` | `false` | Per-person stats (one API call per person) |
| `--topn` / `--fanout.limit` | `TOPN` / `FANOUT_LIMIT` | `25` / `200` | Series cap / max distinct values per breakdown |
| `--log.level` | — | `info` | `debug`, `info`, `warn`, `error` |

<details>
<summary>More: <code>--collect.camera/geo/ratings/heavy</code>, <code>--request-timeout</code>, <code>--fanout.concurrency</code>, <code>--web.config.file</code></summary>

| Flag | Env var | Default | Description |
| --- | --- | --- | --- |
| `--collect.camera` | `COLLECT_CAMERA` | `true` | Per camera make/model/lens breakdowns |
| `--collect.geo` | `COLLECT_GEO` | `true` | Geo distribution (countries, `/map/markers`) |
| `--collect.ratings` | `COLLECT_RATINGS` | `true` | Star-rating breakdown |
| `--collect.heavy` | `COLLECT_HEAVY` | `true` | Duplicates & stacks (larger responses) |
| `--request-timeout` | `REQUEST_TIMEOUT` | `20s` | Per-request HTTP timeout |
| `--fanout.concurrency` | `FANOUT_CONCURRENCY` | `8` | Concurrent fan-out requests |
| `--web.config.file` | — | — | [exporter-toolkit web config](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md) for TLS / basic-auth |

> Legacy `*_SECONDS` env vars (`SCRAPE_INTERVAL_SECONDS`, …) and `EXPORTER_PORT` are still honoured, so existing deployments keep working unchanged.

</details>

### API token

The token is the one secret and is **never** a flag (it would show up in `ps`). Provide it, in order of precedence:

1. `IMMICH_API_TOKEN` environment variable (recommended)
2. `immich.token_file` in YAML — path to a mounted secret file
3. `immich.token` in YAML — inline (discouraged; easy to commit by accident)

Use an **admin** key for full coverage (`immich_key_is_admin` is `1`); a non-admin key degrades gracefully.

## Metrics

Every series is prefixed `immich_` (plus standard `go_*` / `process_*`). Gauges unless noted; counters end in `_total`, byte values in `_bytes`. Roughly grouped:

| Group | Example series |
| --- | --- |
| Exporter & server health | `immich_exporter_up`, `immich_up`, `immich_server_info`, `immich_server_update_available` |
| Assets | `immich_assets`, `immich_assets_by_year`, `immich_assets_by_rating`, `immich_assets_favorite` |
| Cameras & EXIF | `immich_assets_by_camera_make`, `_camera_model`, `_lens` |
| Geography | `immich_assets_by_country`, `immich_assets_by_city` (with `lat`/`lon`), `immich_assets_geotagged` |
| People & users | `immich_people`, `immich_person_assets`, `immich_user_usage_bytes`, `immich_user_quota_bytes` |
| Albums & sharing | `immich_albums`, `immich_album_top_assets`, `immich_shared_links`, `immich_partners` |
| Content & jobs | `immich_tags`, `immich_memories`, `immich_duplicate_sets`, `immich_libraries`, `immich_job_queue` |

Instance-wide volumetrics and job metrics require an **admin** key — `immich_key_is_admin` reports which mode you're in. Scrape `/metrics` for the full set; liveness is at `/healthz`.

## Building

```sh
go build -o immich-exporter ./cmd/immich-exporter
go test -race ./...
golangci-lint run
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the dev workflow and [SECURITY.md](SECURITY.md) for the security policy — notably, don't expose `/metrics` publicly, as it carries library metadata (user/people/album names, geo centroids).

## Disclaimer

Unofficial community project, **not affiliated with or endorsed by the Immich project**.

## License

[Apache-2.0](LICENSE)
