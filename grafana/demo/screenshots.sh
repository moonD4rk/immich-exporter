#!/usr/bin/env bash
# Renders the dashboard over the synthetic demo library (internal/exporter/demo_test.go) into docs/images, in the
# dark theme as dashboard-<band>.webp and the light one as dashboard-<band>-light.webp.
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

# load imports dashboard file $1 as uid $2, narrowed by the jq filter $3.
load() {
  jq --arg uid "$2" "$3"' | .uid = $uid | .title = $uid | .id = null
    | {dashboard: ., overwrite: true,
       inputs: [{name: "DS_PROMETHEUS", type: "datasource", pluginId: "prometheus", value: "prometheus"}]}' "$1" |
    curl -sf -X POST -H 'Content-Type: application/json' -d @- "$grafana/api/dashboards/import" >/dev/null
}
load grafana/dashboard.json immich-library .

# Collapsed rows keep their panels inside the row, so lay every row out expanded before slicing.
jq '.panels |= (reduce .[] as $p ({off: 0, out: []}; . as $s
  | if $p.type == "row" and ($p.panels // [] | length) > 0 then
      ([$p.panels[].gridPos.y] | min) as $base | ($p.gridPos.y + $s.off) as $ry
      | {off: ($s.off + ([$p.panels[] | .gridPos.y - $base + .gridPos.h] | max)),
         out: ($s.out + [$p | .collapsed = false | .panels = [] | .gridPos.y = $ry]
                      + [$p.panels[] | .gridPos.y = $ry + 1 + .gridPos.y - $base])}
    else {off: $s.off, out: ($s.out + [$p | .gridPos.y += $s.off])} end) | .out)' \
  grafana/dashboard.json >"$tmp/expanded.json"

# Each image runs from one row title to the next (empty = the dashboard's edge); a grid row renders 38px
# tall. Only the first keeps the dashboard controls, and every render is cut above Grafana's badge.
# Pages render 1600px wide, where every status tile's title fits, at twice the pixel density for
# high-density screens, and are saved as lossless WebP, a third the size of the same PNG.
width=1600 scale=2
bands=(
  "overview||Places"
  "geography|Places|Cameras & lenses"
  "cameras|Cameras & lenses|People"
  "people|People|Albums & sharing"
  "albums|Albums & sharing|Users & storage"
  "users|Users & storage|Jobs"
  "jobs|Jobs|Server & exporter"
  "server|Server & exporter|More"
  "more|More|"
)
controls=
for band in "${bands[@]}"; do
  IFS='|' read -r name from to <<<"$band"
  read -r first end < <(jq -r --arg from "$from" --arg to "$to" '
    def edge($title; $default): [.panels[] | select(.type == "row" and .title == $title) | .gridPos.y][0] // $default;
    [edge($from; 0), edge($to; [.panels[] | .gridPos.y + .gridPos.h] | max)] | @tsv' "$tmp/expanded.json")
  load "$tmp/expanded.json" "shot-$name" ".panels |= map(select(.gridPos.y >= $first and .gridPos.y < $end) | .gridPos.y -= $first)"
  for theme in dark light; do
    out=docs/images/dashboard-$name.webp
    [[ $theme == dark ]] || out=docs/images/dashboard-$name-$theme.webp
    curl -sf -o "$tmp/$name.png" "$grafana/render/d/shot-$name/_?orgId=1&kiosk&theme=$theme&from=now-30d&to=now&width=$width&height=$(((end - first) * 38 + 200))&scale=$scale&timeout=120$controls"
    bg=$(magick "$tmp/$name.png" -format '%[pixel:p{0,0}]' info:)
    magick "$tmp/$name.png" -gravity south -chop "0x$((64 * scale))" -fuzz 2% -trim +repage \
      -bordercolor "$bg" -border $((16 * scale)) -strip -quality 100 -define webp:lossless=true -define webp:method=6 "$out"
    echo "$out"
  done
  controls='&_dash.hideTimePicker&_dash.hideVariables&_dash.hideLinks'
done
if [[ ${KEEP:-} == 1 ]]; then echo "Grafana: http://127.0.0.1:13001/d/immich-library (admin/admin)"; fi
