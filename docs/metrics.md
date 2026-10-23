# Metrics

Every series starts with `immich_`, next to the standard `go_*` and `process_*` series. They are gauges unless noted: counters end in `_total`, and byte values in `_bytes`. Scrape `/metrics` for the full set; `/healthz` answers liveness checks.

| Group | Example series |
| --- | --- |
| Exporter health | `immich_exporter_up`, `immich_exporter_collector_success{collector}`, `immich_exporter_collector_duration_seconds`, `immich_key_is_admin` |
| Server | `immich_up`, `immich_server_info`, `immich_server_maintenance_mode`, `immich_server_update_available`, `immich_server_version_check_timestamp_seconds` |
| Integrity & backups | `immich_integrity_issues{type}`, `immich_database_backups`, `immich_database_backup_last_timestamp_seconds`, `immich_database_backup_last_size_bytes` |
| Assets | `immich_assets`, `immich_assets_by_year`, `immich_assets_by_rating`, `immich_assets_favorite`, `immich_assets_locked`, `immich_assets_by_file_size`¹ |
| Cameras & EXIF | `immich_assets_by_camera_make`, `_camera_model`, `_lens`, `immich_assets_with_camera`¹ |
| Geography | `immich_assets_by_country`, `immich_assets_by_city` (with `lat`/`lon`), `immich_assets_geotagged` |
| People & users | `immich_people`, `immich_person_assets`, `immich_assets_with_people`¹, `immich_user_usage_bytes`, `immich_user_storage_bytes{type}`, `immich_user_quota_bytes` |
| Albums & sharing | `immich_albums`, `immich_album_top_assets`, `immich_shared_links`, `immich_partners` |
| Content & jobs | `immich_tags`, `immich_memories`, `immich_duplicate_sets`, `immich_library_assets`, `immich_library_usage_bytes`, `immich_workflows`, `immich_job_queue` |

¹ Needs Immich 3.2+, whose search filters can express them. They count visible assets (timeline and archive), so they add up to the library size.

## What the API key can see

Instance-wide volumetrics, integrity checks, backups and job metrics need an **admin** key; `immich_key_is_admin` reports which kind is in use. Counts from Immich's search (states, ratings, years, cameras, places, file sizes) cover the key owner's library. Except for ¹, they include the hidden video half of each live photo, so they can exceed `immich_assets`.

## Immich's own metrics

Immich's built-in OpenTelemetry metrics complement these: they measure performance, where this exporter reports content. Enable them with `IMMICH_TELEMETRY_INCLUDE=all` and scrape **both** ports of the server container:

- `8081` for the API: request latency and database query timings.
- `8082` for the microservices worker: job throughput and failures (`immich_jobs_*_total`).

Scraping only `8081` misses every job metric.
