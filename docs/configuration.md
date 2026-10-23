# Configuration

Settings come from four layers, and each overrides the one before it:

**built-in defaults → YAML file → environment variables → command-line flags**

Keep everything in one YAML file and still override a single value with an environment variable or a flag. `immich-exporter --help` lists every flag with its environment variable.

## API key

The API key is the one secret, so it is never a flag, which would show it in `ps`. The exporter reads it from, in order of precedence:

1. The `IMMICH_API_TOKEN` environment variable (recommended).
2. A file named by `--immich.token-file`, `IMMICH_API_TOKEN_FILE` or `immich.token_file` in YAML, such as a mounted secret.
3. `immich.token` in the YAML file. Avoid it, since it is easy to commit by accident.

Create the key in Immich under **Account Settings → API Keys**. An admin key with the `all` permission collects every metric, and `immich_key_is_admin` reports `1`. A non-admin or scoped key still works: whatever it may not read reports `immich_exporter_collector_success 0` under that collector's name, and the exporter logs it once.

## Flags and environment variables

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `--config.file` | `IMMICH_CONFIG_FILE` | — | YAML config file |
| `--immich.base-url` | `IMMICH_BASE_URL` | — | Full API base URL including `/api`; overrides host and port |
| `--immich.host` | `IMMICH_HOST` | `localhost` | Immich server host; the API is served under `/api` |
| `--immich.port` | `IMMICH_PORT` | `2283` | Immich server port |
| `--immich.token-file` | `IMMICH_API_TOKEN_FILE` | — | File holding the API key, such as a mounted secret |
| `--scrape-interval` | `SCRAPE_INTERVAL` | `2m` | How often the cheap counts refresh |
| `--breakdown-interval` | `BREAKDOWN_INTERVAL` | `15m` | How often cameras, places, people, duplicates and stacks refresh |
| `--request-timeout` | `REQUEST_TIMEOUT` | `20s` | Timeout of each API request |
| `--collect.camera` | `COLLECT_CAMERA` | `true` | Camera make, model and lens breakdowns |
| `--collect.geo` | `COLLECT_GEO` | `true` | Countries and cities, from `/map/markers` |
| `--collect.ratings` | `COLLECT_RATINGS` | `true` | Star-rating breakdown |
| `--collect.people` | `COLLECT_PEOPLE` | `false` | Asset counts per person, at one API call per person |
| `--collect.heavy` | `COLLECT_HEAVY` | `true` | Duplicates and stacks, which list the whole library |
| `--topn` | `TOPN` | `25` | Series cap for the model, lens, person and album breakdowns |
| `--fanout.limit` | `FANOUT_LIMIT` | `200` | Skip a breakdown with more distinct values than this |
| `--fanout.concurrency` | `FANOUT_CONCURRENCY` | `8` | Concurrent fan-out requests |
| `--geo.city-limit` | `GEO_CITY_LIMIT` | `200` | Cities kept in `immich_assets_by_city`; the rest sum into `other` (`0` keeps all) |
| `--web.listen-address` | — | `:8000` | Address to listen on |
| `--web.telemetry-path` | — | `/metrics` | Path of the metrics endpoint |
| `--web.config.file` | — | — | [exporter-toolkit web config](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md) for TLS and basic auth |
| `--log.level` | — | `info` | `debug`, `info`, `warn` or `error` |
| `--log.format` | — | `logfmt` | `logfmt` or `json` |

Both intervals must be at least `10s`.

Older deployments keep working unchanged:

- `EXPORTER_PORT` still sets the listen port.
- `SCRAPE_INTERVAL_SECONDS`, `BREAKDOWN_INTERVAL_SECONDS` and `REQUEST_TIMEOUT_SECONDS` still set those durations in whole seconds.

## YAML file

Point the exporter at a file with `--config.file=config.yml` or `IMMICH_CONFIG_FILE`. Every key is optional, and an omitted key keeps its default. An unknown key is rejected, so a typo fails loudly. [`deploy/config.example.yml`](../deploy/config.example.yml) is a full template.

```yaml
immich:
  base_url: http://immich-server:2283/api
web:
  listen_address: ":8000"
scrape: { interval: 2m, breakdown_interval: 15m, request_timeout: 20s }
collect: { camera: true, geo: true, ratings: true, people: false, heavy: true }
limits: { topn: 25, fanout_limit: 200, fanout_concurrency: 8, city_limit: 200 }
```
