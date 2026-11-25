**Needs [immich-exporter](https://github.com/moond4rk/immich-exporter)**, a small Prometheus exporter that reads the Immich API. Its setup, alert rules and metrics reference are on GitHub.

What an [Immich](https://immich.app) library holds: assets, storage, people, places, cameras, albums, jobs, backups and integrity checks.

Immich's built-in telemetry measures how the server performs; this dashboard shows what the library holds. The exporter reads the Immich API with an API key and never touches the database.

## Rows

- **Status**: exporter and API key, last refresh, disk usage and when the disk fills up, queued and failed jobs
- **Library**: assets, photos, videos, library size, people and albums with their growth; new assets, storage by type, assets by year, asset states and star ratings
- **Places**: a world map, geotagged assets, countries, states and cities
- **Cameras & lenses**: makes, models and lenses
- **People**: named, unnamed and hidden people, top people and naming progress
- **Albums & sharing**: albums, shared links and partner sharing
- **Users & storage**: disk usage and storage per user
- **Jobs**: the processing pipeline, backlog over time and every queue
- **Server & exporter**: version, refresh health, integrity checks, database backups, per-collector status, features and settings
- **More**: library extras, file sizes and metadata coverage

## Setup

1. In Immich, create an API key under **Account Settings → API Keys**. An admin account with the `all` permission fills every panel.
2. Run the exporter next to Immich:

   ```yaml
   services:
     immich-exporter:
       image: ghcr.io/moond4rk/immich-exporter:latest
       environment:
         IMMICH_HOST: immich-server
         IMMICH_API_TOKEN: your-api-key
       ports:
         - "8000:8000"
       restart: unless-stopped
   ```

3. Add a scrape job to Prometheus or VictoriaMetrics:

   ```yaml
   scrape_configs:
     - job_name: immich-exporter
       static_configs:
         - targets: ["your-docker-host:8000"]
   ```

4. Import this dashboard and pick your Prometheus data source.

[`deploy/alerts.yml`](https://github.com/moond4rk/immich-exporter/blob/main/deploy/alerts.yml) has alert rules for, among others, a failing job queue, a filling library volume, integrity-check findings and a stale database backup. Every panel query and alert rule is tested on Prometheus and VictoriaMetrics.

The screenshots show a synthetic demo library.

**Source, releases and issues:** https://github.com/moond4rk/immich-exporter. Feedback and panel ideas are welcome there.
