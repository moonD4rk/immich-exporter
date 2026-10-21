#!/usr/bin/env bash
# Renders the dashboard over the synthetic demo library (internal/exporter/demo_test.go) into docs/images.
# Needs Docker, Go, jq and ImageMagick. KEEP=1 leaves Grafana running on http://127.0.0.1:13001 (admin/admin).
set -euo pipefail
cd "$(dirname "$0")/../.."

compose=(docker compose -f grafana/demo/compose.yml)
vm=http://127.0.0.1:18430
grafana=http://admin:admin@127.0.0.1:13001
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"; [[ ${KEEP:-} == 1 ]] || "${compose[@]}" down -v >/dev/null 2>&1' EXIT

"${compose[@]}" up -d --wait >/dev/null
until curl -sf "$grafana/api/health" >/dev/null; do sleep 1; done

# Generated once the stack is up, so the history runs right up to the moment of rendering.
curl -sf "$vm/api/v1/admin/tsdb/delete_series" --data-urlencode 'match[]={job="immich-exporter"}'
DEMO_METRICS="$tmp/demo.prom" go test ./internal/exporter -run '^TestDemoMetrics$' -count=1 >/dev/null
curl -sf --data-binary "@$tmp/demo.prom" \
  "$vm/api/v1/import/prometheus?extra_label=job=immich-exporter&extra_label=instance=immich-exporter:8000"
curl -sf "$vm/internal/force_flush"

# load imports the dashboard as uid $1, narrowed by the jq filter $2.
load() {
  jq --arg uid "$1" "$2"' | .uid = $uid | .title = $uid | .id = null
    | {dashboard: ., overwrite: true,
       inputs: [{name: "DS_PROMETHEUS", type: "datasource", pluginId: "prometheus", value: "prometheus"}]}' \
    grafana/dashboard.json |
    curl -sf -X POST -H 'Content-Type: application/json' -d @- "$grafana/api/dashboards/import" >/dev/null
}
load immich-library .

# Each image is a band of dashboard grid rows, name:first:end; a grid row renders 38px tall. Only the
# first keeps the dashboard controls, and every render is cut above Grafana's badge before trimming.
controls=
for band in overview:0:33 cameras:33:48 geography:48:75 people:75:89 albums:89:109 users:109:130 jobs:139:162; do
  IFS=: read -r name first end <<<"$band"
  load "shot-$name" ".panels |= map(select(.gridPos.y >= $first and .gridPos.y < $end) | .gridPos.y -= $first)"
  curl -sf -o "$tmp/$name.png" "$grafana/render/d/shot-$name/_?orgId=1&kiosk&theme=dark&from=now-30d&to=now&width=1600&height=$(((end - first) * 38 + 200))&timeout=120$controls"
  bg=$(magick "$tmp/$name.png" -format '%[pixel:p{0,0}]' info:)
  magick "$tmp/$name.png" -gravity south -chop 0x64 -fuzz 2% -trim +repage -bordercolor "$bg" -border 16 -strip "docs/images/dashboard-$name.png"
  echo "docs/images/dashboard-$name.png"
  controls='&_dash.hideTimePicker&_dash.hideVariables&_dash.hideLinks'
done
if [[ ${KEEP:-} == 1 ]]; then echo "Grafana: http://127.0.0.1:13001/d/immich-library (admin/admin)"; fi
