# Security Policy

## Reporting a vulnerability

Please **do not** open a public issue for security problems.

Use GitHub's private vulnerability reporting: [**Report a vulnerability**](https://github.com/moonD4rk/immich-exporter/security/advisories/new). You'll get an acknowledgement once the report is triaged, along with a fix or mitigation plan.

## Notes & scope

`immich-exporter` is a read-only exporter, but a few things are worth knowing:

- **The API token is a secret.** It is read only from the `IMMICH_API_TOKEN` environment variable and is never logged or exposed on `/metrics`. Prefer an API key scoped to just the read permissions you need.
- **`/metrics` is unauthenticated and contains library metadata** — user names, people names, album names and geographic centroids. Do not expose the exporter directly to the public internet; scrape it over a private network or place it behind your monitoring stack's auth/proxy.
- **The exporter never writes to Immich.** It issues read requests only (`GET`, and `POST /search/statistics`, which is a read-only count query).

## Supported versions

This project tracks the latest release. Please reproduce issues against the most recent tag (or `main`) before reporting.
