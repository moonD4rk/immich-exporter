// Package grafana defines the Immich Library dashboard; dashboard.json is generated from it.
package grafana

import (
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
)

//go:generate go run gen.go

// immich-logo.svg is Immich's logo (design/immich-logo.svg in immich-app/immich) with its styles inlined and its
// view cropped to the flower.
//
//go:embed immich-logo.svg
var logo string

// JSON renders the dashboard exactly as dashboard.json holds it.
func JSON() ([]byte, error) {
	b, err := json.Marshal(definition(), json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func definition() obj {
	l := &layout{}
	statusStrip(l)
	library(l)
	places(l)
	cameras(l)
	people(l)
	albums(l)
	users(l)
	jobs(l)
	server(l)
	more(l)
	return obj{
		"__inputs": []obj{{
			"name": "DS_PROMETHEUS", "label": "Prometheus", "description": "Prometheus data source scraping immich-exporter",
			"type": "datasource", "pluginId": "prometheus", "pluginName": "Prometheus",
		}},
		"__requires": requires(l.items),
		"annotations": obj{"list": []obj{{
			"builtIn": 1, "datasource": obj{"type": "grafana", "uid": "-- Grafana --"}, "enable": true, "hide": true,
			"iconColor": "rgba(0, 211, 255, 1)", "name": "Annotations & Alerts", "type": "dashboard",
		}}},
		"description": "Immich library content from immich-exporter (https://github.com/moond4rk/immich-exporter): " +
			"assets, cameras and EXIF, geography, people, storage, albums and sharing, jobs and server health.",
		"editable":             true,
		"fiscalYearStartMonth": 0,
		"graphTooltip":         1,
		"id":                   nil,
		"links": []obj{{
			"title": "immich-exporter", "type": "link", "url": "https://github.com/moond4rk/immich-exporter",
			"tooltip": "Exporter setup, metrics reference and issues", "icon": "external link", "targetBlank": true,
			"asDropdown": false, "includeVars": false, "keepTime": false, "tags": []string{},
		}},
		"panels":        l.items,
		"refresh":       "5m",
		"schemaVersion": 41,
		"tags":          []string{"immich", "photos", "library"},
		"templating": obj{"list": []obj{{
			// ${DS_PROMETHEUS} makes an import use the datasource picked in its dialog.
			"name": "datasource", "type": "datasource", "query": "prometheus", "label": "Data source",
			"current": obj{"selected": false, "text": "Prometheus", "value": "${DS_PROMETHEUS}"},
			"hide":    0, "refresh": 1, "regex": "",
		}}},
		"time":       obj{"from": "now-7d", "to": "now"},
		"timepicker": obj{},
		"timezone":   "browser",
		"title":      "Immich Library",
		"uid":        "immich-library",
		"version":    1,
		"weekStart":  "",
	}
}

// requires lists Grafana, the datasource and every panel plugin the dashboard uses, for the import dialog.
func requires(items []any) []obj {
	names := map[string]string{
		"barchart": "Bar chart", "bargauge": "Bar gauge", "canvas": "Canvas", "gauge": "Gauge", "geomap": "Geomap",
		"piechart": "Pie chart", "stat": "Stat", "table": "Table", "timeseries": "Time series",
	}
	var used []string
	add := func(p *panel) {
		if !slices.Contains(used, p.Type) {
			used = append(used, p.Type)
		}
	}
	for _, item := range items {
		switch it := item.(type) {
		case *panel:
			add(it)
		case *row:
			for _, p := range it.Panels {
				add(p)
			}
		}
	}
	slices.Sort(used)
	out := []obj{
		{"type": "grafana", "id": "grafana", "name": "Grafana", "version": "11.0.0"},
		{"type": "datasource", "id": "prometheus", "name": "Prometheus", "version": "1.0.0"},
	}
	for _, t := range used {
		out = append(out, obj{"type": "panel", "id": t, "name": names[t], "version": ""})
	}
	return out
}

// logoURI inlines the logo as text, not base64: Canvas prefixes a fixed image path with Grafana's build directory
// unless the path contains ":/", which the SVG namespace URL supplies.
func logoURI() string {
	return "data:image/svg+xml;utf8," + strings.ReplaceAll(strings.TrimSpace(logo), "#", "%23")
}

// brand heads the status strip: Immich's logo, whether its API answers, and the server version. It is one tile
// wide, so the name, status and version stack beside the logo, and a long version still fits.
func brand() *panel {
	const up, version, update = "Value #A", "version", "Value #B"
	p := canvas("", "Whether the exporter could reach the Immich API on its last poll, and the server version, "+
		"orange when a newer release is available (needs Immich's version check enabled).",
		[]obj{
			image("Logo", logoURI(), pinned(4, 0, 50, 50)),
			label("Name", "Immich", 24, "left", pinned(64, 17, 110, 30)),
			dot("Status light", up, pinned(66, -2, 8, 8)),
			reading("Status", up, up, "No data", 12, "left", pinned(80, -5, 80, 16)),
			// Canvas shows "Double click to set text" for empty text, so a missing version is a zero-width space.
			reading("Version", version, update, "\u200b", 12, "left", pinned(64, -24, 110, 16)),
		},
		frame("min(immich_up)"),
		frame("max by (version) (immich_server_info) * on () group_left () "+
			"(max(immich_server_update_available) or vector(0))"))
	p.Transparent = true
	// An empty result still has the field, so noValue covers what the reading's fallback can't.
	return p.override(up, prop("mappings", valueMappings(when(0, "Offline", red), when(1, "Online", green))),
		prop("thresholds", steps(red, at(1, green))), prop("noValue", "No data")).
		override(update, prop("thresholds", steps(text, at(1, orange))))
}

func statusStrip(l *layout) {
	l.line(3,
		span(3, brand()),
		span(3, status("Exporter", "Whether immich-exporter's last poll succeeded (immich_exporter_up).",
			"min(immich_exporter_up)", "Healthy", "Failing")),
		span(3, stat("API key", "Instance-wide, per-user and job metrics need an admin API key; a non-admin key leaves them empty.",
			latest("min(immich_key_is_admin)", "")).unit("none").
			mappings(when(0, "Not admin", orange), when(1, "Admin", green)).thresholds(orange, at(1, green))),
		span(3, stat("Last refresh", "Time since the exporter last refreshed its snapshot. It polls every 2 minutes by default.",
			latest("time() - max(immich_exporter_last_success_timestamp_seconds)", "")).unit(duration).decimals(0).
			thresholds(green, at(600, orange), at(1800, red))),
		span(3, stat("Disk", "Used share of the volume that holds the library.", latest(diskUsed, "")).
			unit("percentunit").decimals(0).thresholds(green, at(0.8, orange), at(0.9, red))),
		span(3, stat("Disk full in", "When the library volume fills up at the past week's growth rate.",
			latest(diskFullIn, "Full in")).override("Full in", fullIn()...)),
		span(3, count("Queued jobs", "Jobs waiting or delayed across all Immich queues. Spikes after uploads are normal.",
			`sum(max by (queue, state) (immich_job_queue{state=~"waiting|delayed"}))`, text)),
		span(3, stat("Failed jobs", "Jobs Immich currently holds as failed, across all queues.",
			latest(`sum(max by (queue) (immich_job_queue{state="failed"}))`, "")).thresholds(green, at(1, orange))),
	)
}

func library(l *layout) {
	l.section("Library", false)
	l.line(4,
		span(4, kpi("Assets", "Photos and videos in the library. The percentage is the change over the selected time range.",
			"sum(max by (type) (immich_assets))")),
		span(4, kpi("Photos", "Image assets.", `max(immich_assets{type="IMAGE"})`)),
		span(4, kpi("Videos", "Video assets.", `max(immich_assets{type="VIDEO"})`).color(purple)),
		span(4, kpi("Library size", "Storage used by the library's originals and generated files.",
			"max(immich_server_usage_bytes)").unit("bytes")),
		span(4, kpi("People", "Recognized people, named or not.", "max(immich_people)")),
		span(4, kpi("Albums", "Albums, shared or private.", "sum(max by (shared) (immich_albums))")),
	)
	l.line(9,
		span(16, assetTypes(timeseries("New assets",
			"Net change in photos and videos per interval: uploads and imports minus deletions.",
			seriesStyle{stacked: true, bars: true, signed: true},
			series("max by (type) (immich_assets) - max by (type) (immich_assets offset $__interval)", "{{type}}")).every("1d"))),
		span(8, storageByType()),
	)
	l.line(9,
		span(16, assetsByYear()),
		span(8, bars("Asset states", "Assets by state. Offline assets are missing on disk and turn red.",
			latest("max(immich_assets_favorite)", "Favorites"),
			latest("max(immich_assets_archived)", "Archived"),
			latest("max(immich_assets_not_in_album)", "Not in an album"),
			latest("max(immich_assets_motion)", "Motion photos"),
			latest("max(immich_assets_encoded)", "Transcoded videos"),
			latest("max(immich_assets_hidden)", "Hidden"),
			latest("max(immich_assets_trashed)", "In trash"),
			latest("max(immich_assets_offline)", "Offline")).
			override("Offline", alarm(red)...)),
	)
	l.line(7,
		span(8, bars("Star ratings", "Rated assets by star rating. Unrated assets are left out so the scale stays readable.",
			latest(`max by (rating) (immich_assets_by_rating{rating!="unrated"})`, "{{rating}} ★"))),
		span(8, rings("Organized", "Share of assets with a location, share of assets in at least one album, "+
			"and share of recognized people with a name.",
			latest("max(immich_assets_geotagged) / clamp_min(sum(max by (type) (immich_assets)), 1)", "Geotagged"),
			latest("1 - max(immich_assets_not_in_album) / clamp_min(sum(max by (type) (immich_assets)), 1)", "In an album"),
			latest("max(immich_people_named) / clamp_min(max(immich_people), 1)", "People named"))),
		span(8, timeseries("Library size", "Storage used by the library over time.", seriesStyle{noLegend: true, signed: true},
			series("max(immich_server_usage_bytes)", "Library")).unit("bytes").color(blue)),
	)
}

func storageByType() *panel {
	p := newPanel("piechart", "Storage by type", "Bytes used by photos versus videos.",
		latest("max by (type) (immich_assets_storage_bytes)", "{{type}}"))
	p.set("unit", "bytes").set("color", obj{"mode": "palette-classic"}).set("custom", obj{"hideFrom": hideNothing()})
	p.Options = obj{
		"reduceOptions": reduceLast(), "pieType": "donut", "displayLabels": []string{},
		"tooltip": obj{"mode": "single", "sort": "none"},
		"legend":  obj{"showLegend": true, "displayMode": "table", "placement": "bottom", "values": []string{"value", "percent"}},
	}
	return assetTypes(p)
}

func assetsByYear() *panel {
	p := newPanel("barchart", "Assets by year", "Assets by the year they were taken (EXIF capture date).",
		frame("max by (year) (immich_assets_by_year)"))
	p.set("color", fixed(blue)).set("thresholds", steps(blue)).set("custom", obj{
		"lineWidth": 0, "fillOpacity": 80, "gradientMode": "none", "axisPlacement": "auto", "axisLabel": "",
		"axisBorderShow": false, "axisCenteredZero": false, "axisSoftMin": 0, "scaleDistribution": obj{"type": "linear"},
		"thresholdsStyle": obj{"mode": "off"}, "hideFrom": hideNothing(),
	})
	p.Options = obj{
		"orientation": "vertical", "xField": "year", "showValue": "always", "stacking": "none",
		"groupWidth": 0.7, "barWidth": 0.8, "barRadius": 0.05, "fullHighlight": false,
		"xTickLabelRotation": 0, "xTickLabelSpacing": 0, "text": obj{"valueSize": 11},
		"legend":  obj{"showLegend": false, "displayMode": "list", "placement": "bottom", "calcs": []string{}},
		"tooltip": obj{"mode": "single", "sort": "none"},
	}
	return p.override("Assets", prop("unit", "short")).
		override("year", prop("unit", "none"), prop("decimals", 0)).
		transform(rename([]string{"Time"}, col("Value", "Assets")),
			obj{"id": "sortBy", "options": obj{"sort": []obj{{"field": "year", "desc": false}}}})
}

func places(l *layout) {
	l.section("Places", false)
	// The geotagged share is a ring of Organized, just above.
	l.line(3,
		span(6, count("Geotagged assets", "Assets with GPS coordinates.", "max(immich_assets_geotagged)", blue)),
		span(6, count("Without location", "Assets with no GPS coordinates.",
			"sum(max by (type) (immich_assets)) - max(immich_assets_geotagged)", text)),
		span(4, count("Countries", "Distinct countries in reverse-geocoded EXIF.", "max(immich_places_countries)", blue)),
		span(4, count("States / provinces", "Distinct states or provinces.", "max(immich_places_states)", blue)),
		span(4, count("Cities", "Distinct cities.", "max(immich_places_cities)", blue)),
	)
	l.line(14,
		span(11, worldMap()),
		span(5, ranking("Top countries", "Countries with the most geotagged assets.",
			`topk(15, max by (country) (immich_assets_by_country{country!="unknown"}))`, col("country", "Country"))),
		span(8, ranking("Top cities", "Cities with the most geotagged assets.",
			`topk(15, max by (city, country) (immich_assets_by_city{city!="unknown"}))`,
			col("city", "City"), col("country", "Country"))),
	)
}

func worldMap() *panel {
	markers := func(name, refID string, legend bool, opacity float64, size obj) obj {
		value := "Value #" + refID
		size["field"] = value
		return obj{
			"type": "markers", "name": name, "filterData": obj{"id": "byRefId", "options": refID},
			"location": obj{"mode": "coords", "latitude": "lat", "longitude": "lon"}, "tooltip": true,
			"config": obj{"showLegend": legend, "style": obj{
				"symbol":      obj{"mode": "fixed", "fixed": "img/icons/marker/circle.svg"},
				"symbolAlign": obj{"horizontal": "center", "vertical": "center"},
				"opacity":     opacity, "color": obj{"field": value}, "size": size,
			}},
		}
	}
	p := newPanel("geomap", "Map", "Countries as large bubbles, cities as small dots, both sized and colored by asset count. "+
		"Each sits at the centroid of its own geotagged assets.",
		target{
			expr: `max by (country, lat, lon) (immich_assets_by_country{country!="unknown"})`, legend: "{{country}}",
			format: "table", instant: true,
		},
		target{
			expr: `max by (city, country, lat, lon) (immich_assets_by_city{city!="unknown"})`, legend: "{{city}}",
			format: "table", instant: true,
		})
	p.set("unit", "short").set("color", obj{"mode": "continuous-BlPu"}).set("thresholds", steps(blue)).
		set("custom", obj{"hideFrom": hideNothing()})
	p.Options = obj{
		"view":    obj{"id": "fit", "lat": 20, "lon": 0, "zoom": 1, "allLayers": true},
		"basemap": obj{"type": "default", "name": "Basemap"},
		"controls": obj{
			"showZoom": true, "mouseWheelZoom": false, "showAttribution": true, "showScale": false,
			"showMeasure": false, "showDebug": false,
		},
		"tooltip": obj{"mode": "details"},
		"layers": []obj{
			markers("Countries", "A", true, 0.45, obj{"fixed": 5, "min": 8, "max": 44}),
			markers("Cities", "B", false, 0.9, obj{"fixed": 3, "min": 3, "max": 10}),
		},
	}
	return p.transform(obj{"id": "convertFieldType", "options": obj{"conversions": []obj{
		{"targetField": "lat", "destinationType": "number"},
		{"targetField": "lon", "destinationType": "number"},
	}}})
}

func cameras(l *layout) {
	l.section("Cameras & lenses", true)
	l.line(3,
		span(8, count("Makes", "Distinct camera makes in EXIF.", "max(immich_camera_makes)", blue)),
		span(8, count("Models", "Distinct camera models in EXIF.", "max(immich_camera_models)", blue)),
		span(8, count("Lenses", "Distinct lenses in EXIF.", "max(immich_lenses)", blue)),
	)
	l.line(11,
		span(8, ranking("Top camera models", "Assets per camera model.",
			"topk(15, max by (model) (immich_assets_by_camera_model))", col("model", "Model"))),
		span(10, ranking("Top lenses", "Assets per lens.", "topk(15, max by (lens) (immich_assets_by_lens))", col("lens", "Lens"))),
		span(6, ranking("Camera makes", "Assets per camera make.", "max by (make) (immich_assets_by_camera_make)", col("make", "Make"))),
	)
}

func people(l *layout) {
	l.section("People", true)
	l.line(3,
		span(4, count("People", "Recognized people.", "max(immich_people)", blue)),
		span(4, count("Named", "People with a name.", "max(immich_people_named)", blue)),
		span(4, count("Unnamed", "People still to be named.", "max(immich_people_unnamed)", text)),
		span(4, count("Hidden", "People hidden from the people view.", "max(immich_people_hidden)", text)),
		span(4, count("With birthday", "People with a birth date set.", "max(immich_people_with_birthdate)", blue)),
		span(4, stat("Named share", "Share of recognized people that have a name.",
			latest("max(immich_people_named) / clamp_min(max(immich_people), 1)", "")).unit("percentunit").decimals(1).color(blue)),
	)
	l.line(10,
		span(12, ranking("Top people", "People appearing in the most assets. Needs COLLECT_PEOPLE=true.",
			"topk(15, max by (person) (immich_person_assets))", col("person", "Person"))),
		span(12, timeseries("Naming progress", "Named versus unnamed people over time.", seriesStyle{stacked: true},
			series("max(immich_people_named)", "Named"), series("max(immich_people_unnamed)", "Unnamed")).
			override("Named", prop("color", fixed(blue))).override("Unnamed", prop("color", fixed(text)))),
	)
}

func albums(l *layout) {
	l.section("Albums & sharing", true)
	l.line(3,
		span(4, count("Albums", "All albums.", "sum(max by (shared) (immich_albums))", blue)),
		span(4, count("Shared", "Albums shared with other users.", "max(immich_albums_shared)", blue)),
		span(4, count("Private", "Albums not shared with anyone.", "max(immich_albums_not_shared)", blue)),
		span(4, count("Empty", "Albums with no assets.", "max(immich_albums_empty)", text)),
		span(4, count("Average size", "Average assets per album.", "max(immich_album_assets_avg)", blue).decimals(0)),
		span(4, count("Largest", "Assets in the largest album.", "max(immich_album_assets_max)", blue)),
	)
	l.line(10,
		span(12, ranking("Top albums", "Albums with the most assets.", "topk(10, max by (album) (immich_album_top_assets))",
			col("album", "Album"))),
		span(8, bars("Shared links", "Public share links and their protection. Links that never expire turn orange.",
			latest(`max(immich_shared_links{type="ALBUM"})`, "Album links"),
			latest(`max(immich_shared_links{type="INDIVIDUAL"})`, "Asset links"),
			latest("max(immich_albums_with_shared_link)", "Albums with a link"),
			latest("max(immich_shared_links_password_protected)", "Password protected"),
			latest("max(immich_shared_links_never_expire)", "Never expire"),
			latest("max(immich_shared_links_expired)", "Expired")).
			override("Never expire", alarm(orange)...)),
		span(4, bars("Partner sharing", "Partner libraries shared with you (incoming) and by you (outgoing).",
			latest("max by (direction) (immich_partners)", "{{direction}}"))),
	)
}

func users(l *layout) {
	// Scoped to E: with a single user the table queries look like time series too and would become trends.
	growth := obj{
		"id": "timeSeriesTable", "filter": obj{"id": "byRefId", "options": "E"},
		"options": obj{"E": obj{"timeField": "Time", "stat": "lastNotNull"}},
	}
	l.section("Users & storage", true)
	l.line(8,
		span(4, diskUsage()),
		span(4, diskSpace()),
		span(4, grid("Accounts", "Users by status and role.",
			latest("max by (status) (immich_users)", "{{status}}"),
			latest("max by (role) (immich_users_by_role)", "{{role}}s")).opt("orientation", "horizontal")),
		span(12, table("Users", "Assets and storage per user, and how their storage grew over the selected range.",
			frame("max by (user) (immich_user_photos)"),
			frame("max by (user) (immich_user_videos)"),
			frame("max by (user) (immich_user_usage_bytes)"),
			frame("max by (user) (immich_user_quota_unlimited)"),
			series("max by (user) (immich_user_usage_bytes)", "{{user}}")).
			transform(growth, obj{"id": "merge", "options": obj{}}, organize([]string{"Time"},
				col("user", "User"), col("Value #A", "Photos"), col("Value #B", "Videos"),
				col("Value #C", "Storage"), col("Trend #E", "Growth"), col("Value #D", "Quota"))).
			override("Photos", prop("custom.width", 80)).
			override("Videos", prop("custom.width", 70)).
			override("Storage", barCell("bytes")...).
			override("Growth", prop("custom.cellOptions", obj{"type": "sparkline", "hideValue": true}),
				prop("custom.width", 110), prop("color", fixed(blue))).
			override("Quota", prop("mappings", valueMappings(when(0, "Limited", text), when(1, "Unlimited", text))),
				prop("custom.width", 90)).
			sortBy("Storage")),
	)
}

const (
	diskUsed = "max(immich_storage_used_bytes) / clamp_min(max(immich_storage_size_bytes), 1)"
	// diskFullIn is the seconds until the volume fills up at its average growth over the last week, and Inf while it
	// isn't growing; without storage metrics it stays empty rather than claiming the volume isn't filling.
	diskFullIn = "max(immich_storage_available_bytes) / (max(deriv(immich_storage_used_bytes[7d])) > 0)" +
		" or (max(immich_storage_available_bytes) + Inf)"
)

// fullIn shows a forecast in seconds as the time left: red within a month, orange within a quarter, and Not filling
// from a century on, which includes the Inf of a volume that isn't growing.
func fullIn() []obj {
	const day = 24 * 60 * 60
	return []obj{
		prop("unit", duration), prop("decimals", 0), prop("color", obj{"mode": "thresholds"}),
		prop("thresholds", steps(red, at(30*day, orange), at(90*day, text))),
		prop("mappings", []obj{{"type": "range", "options": obj{
			"from": 100 * 365 * day, "to": nil, "result": obj{"text": "Not filling", "color": text, "index": 0},
		}}}),
	}
}

func diskUsage() *panel {
	return gauge("Disk usage", "Used share of the volume that holds the library, and its trend over the selected range.",
		series(diskUsed, "Used")).
		unit("percentunit").decimals(1).set("min", 0).set("max", 1).thresholds(green, at(0.8, orange), at(0.9, red))
}

func diskSpace() *panel {
	return grid("Disk space", "Used, free and total space on the library volume, and when it fills up at "+
		"the past week's growth rate.",
		latest("max(immich_storage_used_bytes)", "Used"),
		latest("max(immich_storage_available_bytes)", "Free"),
		latest("max(immich_storage_size_bytes)", "Total"),
		latest(diskFullIn, "Full in")).
		unit("bytes").opt("orientation", "horizontal").
		override("Full in", fullIn()...)
}

// stage is a box of the pipeline: an Immich queue, or the uploads that start the pipeline, and the boxes it feeds.
type stage struct {
	name, field string
	col         int
	row         float64
	feeds       []string
}

// pipelineStages follow an upload through Immich's processing queues in the order Immich chains their jobs.
var pipelineStages = []stage{
	{"Uploads · 1h", "uploads", 0, 1.5, []string{"Metadata"}},
	{"Metadata", "metadataExtraction", 1, 1.5, []string{"Storage template"}},
	{"Storage template", "storageTemplateMigration", 2, 1.5, []string{"Thumbnails"}},
	{"Thumbnails", "thumbnailGeneration", 3, 1.5, []string{"Smart search", "Faces", "OCR", "Transcoding"}},
	{"Smart search", "smartSearch", 4, 0, []string{"Duplicates"}},
	{"Faces", "faceDetection", 4, 1, []string{"Recognition"}},
	{"OCR", "ocr", 4, 2, nil},
	{"Transcoding", "videoConversion", 4, 3, nil},
	{"Duplicates", "duplicateDetection", 5, 0, nil},
	{"Recognition", "facialRecognition", 5, 1, nil},
}

// pipeline draws pipelineStages. A box shows its queue's backlog, or a dash for a queue the server doesn't have, and
// turns blue while there is a backlog; the link into a queue moves while that queue is running, so the panel is still
// when Immich is idle.
func pipeline() *panel {
	field := map[string]string{}
	for _, s := range pipelineStages {
		field[s.name] = s.field
	}
	const width, height = 13.6, 20.5
	var elements []obj
	for _, s := range pipelineStages {
		left, top := 1+16.6*float64(s.col), 3+24.5*s.row
		box := element("rectangle", s.name, scaled(left, top, width, height), obj{"text": obj{"mode": "fixed", "fixed": ""}})
		box["background"] = obj{"color": fieldColor(s.field, "transparent")}
		box["border"] = obj{"color": fieldColor(s.field+" outline", faint), "width": 1, "radius": 8}
		var links []obj
		for _, next := range s.feeds {
			// Canvas animates a link always or never, so the moving one stays transparent until its queue runs.
			flow := link(next, fieldColor(field[next]+" running", "transparent"), 3)
			flow["lineStyle"] = obj{"style": "dashed", "animate": true}
			links = append(links, link(next, obj{"fixed": faint}, 1.5), flow)
		}
		if links != nil {
			box["connections"] = links
		}
		// Name over count rather than side by side, so a phone-width panel doesn't overlap them.
		elements = append(elements, box,
			label(s.name+" label", s.name, 13, "center", scaled(left, top+1, width, 9)),
			reading(s.name+" backlog", s.field, "", "–", 20, "center", scaled(left, top+9.5, width, 10)))
	}
	const backlog = `sum by (queue) (max by (queue, state) (immich_job_queue{state=~"waiting|delayed|active"}))`
	const uploads = "clamp_min(sum(max by (type) (immich_assets)) - sum(max by (type) (min_over_time(immich_assets[1h]))), 0)"
	return canvas("Pipeline", "How an upload moves through Immich's processing queues, in the order Immich runs them. "+
		"Each box shows the jobs waiting or running in its queue and turns blue while there are any; the link into a "+
		"queue moves while that queue is running. Uploads counts assets added in the last hour.",
		elements,
		latest(backlog, "{{queue}}"),
		latest(backlog, "{{queue}} outline"),
		latest(`max by (queue) (immich_job_queue{state="active"})`, "{{queue}} running"),
		latest(uploads, "uploads"),
		latest(uploads, "uploads outline")).
		unit("short").decimals(0).thresholds("transparent", at(1, tint)).
		overrideMatching(".* outline$", prop("thresholds", steps(faint, at(1, blue)))).
		overrideMatching(".* running$", prop("thresholds", steps("transparent", at(1, blue))))
}

func jobs(l *layout) {
	l.section("Jobs", true)
	l.line(10, span(24, pipeline()))
	l.line(9,
		span(24, timeseries("Backlog over time", "Jobs queued (waiting or delayed) and running, summed over every queue.",
			seriesStyle{},
			series(`sum(max by (queue, state) (immich_job_queue{state=~"waiting|delayed"}))`, "Queued"),
			series(`sum(max by (queue, state) (immich_job_queue{state="active"}))`, "Running")).
			override("Queued", prop("color", fixed(blue))).override("Running", prop("color", fixed(green)))),
	)
	states := []column{col(`queue\state`, "Queue")}
	for _, s := range []string{"waiting", "active", "delayed", "failed", "paused", "completed"} {
		states = append(states, col(s, string(s[0]-'a'+'A')+s[1:]))
	}
	l.line(8,
		span(19, table("Queues", "Every queue by state, as Immich reports it. Zeros show as a dash; failed jobs turn red.",
			frame("max by (queue, state) (immich_job_queue)")).mappings(when(0, "–", "")).
			transform(obj{"id": "groupingToMatrix", "options": obj{
				"columnField": "state", "rowField": "queue", "valueField": "Value", "emptyValue": "zero",
			}}, organize(nil, states...)).
			override("Failed", append([]obj{prop("custom.cellOptions", obj{"type": "color-text"})}, alarm(red)...)...).
			sortBy("Waiting")),
		span(5, stat("Paused queues", "Queues an admin has paused. Their jobs wait until the queue is resumed.",
			latest("sum(max by (queue) (immich_job_queue_paused))", "")).
			thresholds(green, at(1, orange)).opt("text", obj{"valueSize": 48})),
	)
}

func server(l *layout) {
	l.section("Server & exporter", true)
	l.line(4,
		span(18, table("Server", "Immich server build and bundled media tooling.",
			frame("max by (version, build, nodejs, ffmpeg, imagemagick, libvips, exiftool) (immich_server_info)")).
			unit("string").
			transform(organize([]string{"Time", "Value"},
				col("version", "Version"), col("build", "Build"), col("nodejs", "Node.js"), col("ffmpeg", "FFmpeg"),
				col("imagemagick", "ImageMagick"), col("libvips", "libvips"), col("exiftool", "ExifTool")))),
		span(6, stat("Exporter version", "immich-exporter build.",
			latest("max by (version) (immich_exporter_build_info)", "{{version}}")).
			unit("none").color(text).opt("textMode", "name").opt("text", obj{"valueSize": 28})),
	)
	l.line(7,
		span(12, timeseries("Refresh duration", "How long exporter refreshes of the Immich API take: the average "+
			"and the slowest per interval.", seriesStyle{},
			series("max(avg_over_time(immich_exporter_last_scrape_duration_seconds[$__interval]))", "Typical"),
			series("max(max_over_time(immich_exporter_last_scrape_duration_seconds[$__interval]))", "Slowest")).
			unit("s").every("10m").
			override("Typical", prop("color", fixed(blue))).
			override("Slowest", prop("color", fixed(text)), prop("custom.fillOpacity", 0),
				prop("custom.lineStyle", obj{"fill": "dash", "dash": []int{6, 4}}))),
		span(12, timeseries("Refresh errors", "Failed exporter refreshes per interval (at least an hour).",
			seriesStyle{noLegend: true, bars: true},
			series("sum(increase(immich_exporter_scrape_errors_total[$__interval]))", "Errors")).color(red).every("1h")),
	)
	unsnake := obj{"id": "renameByRegex", "options": obj{"regex": "^(.*)_(.*)$", "renamePattern": "$1 $2"}}
	l.line(8,
		span(12, stat("Features", "Server features enabled in Immich.",
			latest("max by (feature) (immich_server_feature)", "{{feature}}")).
			unit("none").mappings(when(0, "Off", text), when(1, "On", green)).
			opt("textMode", "value_and_name").opt("text", obj{"titleSize": 13, "valueSize": 22}).
			transform(unsnake, unsnake)),
		span(12, grid("Settings", "Server settings that shape retention and face grouping.",
			latest("max(immich_config_trash_days)", "Trash retention (days)"),
			latest("max(immich_config_user_delete_delay_days)", "User delete delay (days)"),
			latest("max(immich_config_min_faces)", "Min faces per person"),
			latest("max(immich_server_licensed)", "Licensed"),
			latest("max(immich_server_onboarded)", "Onboarded")).
			unit("none").color(text).opt("orientation", "horizontal").
			override("Licensed", prop("mappings", valueMappings(when(0, "No", text), when(1, "Yes", text)))).
			override("Onboarded", prop("mappings", valueMappings(when(0, "No", text), when(1, "Yes", text))))),
	)
}

func more(l *layout) {
	l.section("More", true)
	l.line(5,
		span(24, grid("Library extras", "Remaining library counters.",
			latest("max(immich_tags)", "Tags"),
			latest("max(immich_tags_root)", "Top-level tags"),
			latest("max(immich_memories)", "Memories"),
			latest("max(immich_duplicate_sets)", "Duplicate groups"),
			latest("max(immich_duplicate_assets)", "Duplicate assets"),
			latest("max(immich_stacks)", "Stacks"),
			latest("max(immich_stacked_assets)", "Stacked assets"),
			latest("max(immich_libraries)", "External libraries"),
			latest("sum(max by (library) (immich_library_assets))", "External library assets"),
			latest("max(immich_api_keys)", "API keys"),
			latest("max(immich_sessions)", "Sessions"),
			latest("max(immich_notifications_unread)", "Unread notifications"))),
	)
}
