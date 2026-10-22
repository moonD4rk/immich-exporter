package grafana

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
)

// obj is a JSON object of Grafana's dashboard model. Options stay untyped so that settings from newer
// Grafana releases, which older releases ignore, need no support here.
type obj = map[string]any

// Colors carry meaning: blue for volume, purple for a second category, text for neutral counts,
// and green, orange and red only for health.
const (
	blue   = "blue"
	purple = "purple"
	green  = "green"
	orange = "orange"
	red    = "red"
	text   = "text"
	// faint and tint are fixed, so they suit both themes: a grey outline and Grafana's blue as a light wash.
	faint = "rgba(128, 128, 128, 0.35)"
	tint  = "rgba(87, 148, 242, 0.16)"
)

const gridWidth = 24

// duration formats seconds as words with correct plurals ("1 minute", "3 weeks"), where "s" prints "1 mins".
const duration = "dtdurations"

func datasource() obj { return obj{"type": "prometheus", "uid": "${datasource}"} }

func hideNothing() obj { return obj{"legend": false, "tooltip": false, "viz": false} }

type target struct {
	expr, legend, format string
	instant              bool
}

// series is a range query, for panels that draw history.
func series(expr, legend string) target { return target{expr: expr, legend: legend} }

// latest is an instant query, for panels that show only the newest value.
func latest(expr, legend string) target { return target{expr: expr, legend: legend, instant: true} }

// frame is an instant query in table format, whose labels become columns.
func frame(expr string) target { return target{expr: expr, instant: true, format: "table"} }

func (t target) json(refID string) obj {
	o := obj{"datasource": datasource(), "expr": t.expr, "instant": t.instant, "range": !t.instant, "refId": refID}
	if t.legend != "" {
		o["legendFormat"] = t.legend
	}
	if t.format != "" {
		o["format"] = t.format
	}
	return o
}

type gridPos struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

type fieldConfig struct {
	Defaults  obj   `json:"defaults"`
	Overrides []obj `json:"overrides"`
}

type panel struct {
	ID              int         `json:"id"`
	Type            string      `json:"type"`
	Title           string      `json:"title"`
	Description     string      `json:"description,omitempty"`
	Datasource      obj         `json:"datasource"`
	Targets         []obj       `json:"targets"`
	Interval        string      `json:"interval,omitempty"`
	FieldConfig     fieldConfig `json:"fieldConfig"`
	Options         obj         `json:"options"`
	Transformations []obj       `json:"transformations,omitempty"`
	Transparent     bool        `json:"transparent,omitempty"`
	GridPos         gridPos     `json:"gridPos"`
}

type row struct {
	ID        int      `json:"id"`
	Type      string   `json:"type"`
	Title     string   `json:"title"`
	Collapsed bool     `json:"collapsed"`
	GridPos   gridPos  `json:"gridPos"`
	Panels    []*panel `json:"panels"`
}

func newPanel(kind, title, desc string, targets ...target) *panel {
	p := &panel{
		Type: kind, Title: title, Description: desc, Datasource: datasource(),
		FieldConfig: fieldConfig{Defaults: obj{}, Overrides: []obj{}}, Options: obj{},
	}
	for i, t := range targets {
		p.Targets = append(p.Targets, t.json(string(rune('A'+i))))
	}
	return p
}

func (p *panel) set(key string, v any) *panel {
	p.FieldConfig.Defaults[key] = v
	return p
}

func (p *panel) opt(key string, v any) *panel {
	p.Options[key] = v
	return p
}

func (p *panel) unit(u string) *panel { return p.set("unit", u) }

func (p *panel) decimals(n int) *panel { return p.set("decimals", n) }

func (p *panel) color(c string) *panel { return p.set("color", fixed(c)) }

func (p *panel) thresholds(base string, levels ...level) *panel {
	return p.set("thresholds", steps(base, levels...))
}

func (p *panel) mappings(ms ...mapping) *panel { return p.set("mappings", valueMappings(ms...)) }

func (p *panel) override(name string, props ...obj) *panel {
	p.FieldConfig.Overrides = append(p.FieldConfig.Overrides, byName(name, props...))
	return p
}

func (p *panel) overrideMatching(pattern string, props ...obj) *panel {
	p.FieldConfig.Overrides = append(p.FieldConfig.Overrides,
		obj{"matcher": obj{"id": "byRegexp", "options": pattern}, "properties": props})
	return p
}

func (p *panel) transform(ts ...obj) *panel {
	p.Transformations = append(p.Transformations, ts...)
	return p
}

// every sets the panel's minimum interval, which is also the floor of $__interval.
func (p *panel) every(interval string) *panel {
	p.Interval = interval
	return p
}

func fixed(c string) obj { return obj{"mode": "fixed", "fixedColor": c} }

type level struct {
	color string
	from  float64
}

func at(from float64, color string) level { return level{color: color, from: from} }

func steps(base string, levels ...level) obj {
	s := []obj{{"color": base, "value": nil}}
	for _, l := range levels {
		s = append(s, obj{"color": l.color, "value": l.from})
	}
	return obj{"mode": "absolute", "steps": s}
}

type mapping struct {
	value       int
	text, color string
}

func when(value int, txt, color string) mapping {
	return mapping{value: value, text: txt, color: color}
}

func valueMappings(ms ...mapping) []obj {
	options := obj{}
	for i, m := range ms {
		o := obj{"text": m.text, "index": i}
		if m.color != "" {
			o["color"] = m.color
		}
		options[strconv.Itoa(m.value)] = o
	}
	return []obj{{"type": "value", "options": options}}
}

func prop(id string, v any) obj { return obj{"id": id, "value": v} }

func byName(name string, props ...obj) obj {
	return obj{"matcher": obj{"id": "byName", "options": name}, "properties": props}
}

func reduceLast() obj { return obj{"values": false, "calcs": []string{"lastNotNull"}, "fields": ""} }

// stat shows one value per series, colored by thresholds that default to neutral text.
func stat(title, desc string, targets ...target) *panel {
	p := newPanel("stat", title, desc, targets...)
	p.set("unit", "short").set("color", obj{"mode": "thresholds"}).set("thresholds", steps(text))
	p.Options = obj{
		"reduceOptions": reduceLast(), "orientation": "auto", "textMode": "value", "wideLayout": true,
		"justifyMode": "auto", "colorMode": "value", "graphMode": "none",
		"showPercentChange": false, "percentChangeColorMode": "same_as_value",
	}
	return p
}

// count is a neutral stat of one instant value.
func count(title, desc, expr, color string) *panel {
	return stat(title, desc, latest(expr, "")).color(color)
}

// status maps a 0/1 health series to words, red when down.
func status(title, desc, expr, ok, bad string) *panel {
	return stat(title, desc, latest(expr, "")).unit("none").
		mappings(when(0, bad, red), when(1, ok, green)).thresholds(red, at(1, green))
}

// kpi is a headline number with its trend and its change over the selected range.
func kpi(title, desc, expr string) *panel {
	p := stat(title, desc, series(expr, "")).color(blue)
	p.Options["colorMode"] = "none"
	p.Options["graphMode"] = "area"
	p.Options["showPercentChange"] = true
	return p
}

// grid lists several labeled values in one panel, for leftover single numbers.
func grid(title, desc string, targets ...target) *panel {
	p := stat(title, desc, targets...).color(blue)
	p.Options["textMode"] = "value_and_name"
	p.Options["text"] = obj{"titleSize": 13, "valueSize": 26}
	return p
}

type seriesStyle struct {
	stacked, bars, noLegend bool
	signed                  bool // the axis may go below zero
	lineWidth               int
}

func timeseries(title, desc string, style seriesStyle, targets ...target) *panel {
	lineWidth, fill, gradient, drawStyle, stacking := 2, 16, "opacity", "line", "none"
	if style.lineWidth > 0 {
		lineWidth = style.lineWidth
	}
	if style.stacked {
		// Stacked parts of a whole need solid fills to read as areas.
		fill, stacking = 40, "normal"
	}
	if style.bars {
		// No outline: a zero-height bar would still draw one, a dotted line along the axis.
		lineWidth, fill, gradient, drawStyle = 0, 80, "none", "bars"
	}
	custom := obj{
		"drawStyle": drawStyle, "lineInterpolation": "linear", "lineWidth": lineWidth, "fillOpacity": fill,
		"gradientMode": gradient, "showPoints": "never", "pointSize": 4, "spanNulls": true, "insertNulls": false,
		"barAlignment": 0, "stacking": obj{"mode": stacking, "group": "A"}, "axisPlacement": "auto", "axisLabel": "",
		"axisBorderShow": false, "axisCenteredZero": false, "scaleDistribution": obj{"type": "linear"},
		"thresholdsStyle": obj{"mode": "off"}, "hideFrom": hideNothing(),
	}
	if !style.signed {
		custom["axisSoftMin"] = 0
	}
	p := newPanel("timeseries", title, desc, targets...)
	p.set("unit", "short").set("color", obj{"mode": "palette-classic"}).set("custom", custom).set("thresholds", steps(green))
	p.Options = obj{
		"legend":  obj{"showLegend": !style.noLegend, "displayMode": "list", "placement": "bottom", "calcs": []string{}},
		"tooltip": obj{"mode": "multi", "sort": "desc"},
	}
	return p
}

// gauge is a health gauge, an arc in the color of the threshold the value is in. Grafana 13 draws it with a
// rounded bar and a sparkline of the range; earlier releases ignore those options and draw the classic arc.
func gauge(title, desc string, targets ...target) *panel {
	p := newPanel("gauge", title, desc, targets...)
	p.set("color", obj{"mode": "thresholds"})
	p.Options = obj{
		"reduceOptions": reduceLast(), "orientation": "auto", "showThresholdLabels": false, "showThresholdMarkers": true,
		"sizing": "auto", "minVizWidth": 75, "minVizHeight": 75,
		// A gradient would tint a healthy value with the colors of the thresholds above it.
		"shape": "gauge", "barShape": "rounded", "sparkline": true, "effects": obj{"gradient": false},
	}
	return p
}

// rings shows shares of a whole as blue progress rings, one per series. Grafana 13 draws circles; earlier
// releases draw the classic arc.
func rings(title, desc string, targets ...target) *panel {
	p := newPanel("gauge", title, desc, targets...)
	p.set("unit", "percentunit").set("decimals", 0).set("min", 0).set("max", 1).
		set("color", fixed(blue)).set("thresholds", steps(blue))
	p.Options = obj{
		"reduceOptions": reduceLast(), "orientation": "auto", "showThresholdLabels": false, "showThresholdMarkers": false,
		"sizing": "auto", "minVizWidth": 60, "minVizHeight": 60,
		"shape": "circle", "barShape": "rounded", "barWidthFactor": 0.35, "sparkline": false, "endpointMarker": "none",
		"effects": obj{"gradient": false},
	}
	return p
}

// bars is a horizontal bar gauge: one bar per series, all in one color on a common scale.
func bars(title, desc string, targets ...target) *panel {
	p := newPanel("bargauge", title, desc, targets...)
	p.set("unit", "short").set("min", 0).set("color", fixed(blue)).set("thresholds", steps(blue))
	p.Options = obj{
		"reduceOptions": reduceLast(), "orientation": "horizontal", "displayMode": "basic", "valueMode": "text",
		"namePlacement": "left", "showUnfilled": true, "sizing": "manual",
		"minVizWidth": 8, "minVizHeight": 16, "maxVizHeight": 22,
	}
	return p
}

// alarm turns one bar or cell of a neutral panel to color once it reaches 1.
func alarm(color string) []obj {
	return []obj{prop("color", obj{"mode": "thresholds"}), prop("thresholds", steps(text, at(1, color)))}
}

func table(title, desc string, targets ...target) *panel {
	p := newPanel("table", title, desc, targets...)
	p.set("unit", "short").set("color", fixed(blue)).set("thresholds", steps(blue)).set("custom", obj{
		"align": "auto", "cellOptions": obj{"type": "auto"}, "inspect": false, "filterable": false,
	})
	p.Options = obj{
		"showHeader": true, "cellHeight": "sm",
		"footer": obj{"show": false, "reducer": []string{"sum"}, "countRows": false, "fields": ""},
	}
	return p
}

func (p *panel) sortBy(column string) *panel {
	return p.opt("sortBy", []obj{{"displayName": column, "desc": true}})
}

// barCell draws a column as inline bars, so long names truncate while values stay legible.
func barCell(unit string) []obj {
	props := []obj{
		prop("custom.cellOptions", obj{"type": "gauge", "mode": "basic", "valueDisplayMode": "text"}),
		prop("color", fixed(blue)), prop("custom.minWidth", 150),
	}
	if unit != "" {
		props = append(props, prop("unit", unit))
	}
	return props
}

type column struct{ label, header string }

func col(label, header string) column { return column{label: label, header: header} }

// organize drops, renames and orders table columns; columns keep their order in the list.
func organize(exclude []string, columns ...column) obj {
	index := obj{}
	for i, c := range columns {
		index[c.label] = i
	}
	return organizeColumns(exclude, columns, index)
}

// rename drops and renames table columns and leaves their order alone.
func rename(exclude []string, columns ...column) obj { return organizeColumns(exclude, columns, obj{}) }

func organizeColumns(exclude []string, columns []column, index obj) obj {
	excluded, renamed := obj{}, obj{}
	for _, name := range exclude {
		excluded[name] = true
	}
	for _, c := range columns {
		renamed[c.label] = c.header
	}
	return obj{"id": "organize", "options": obj{"excludeByName": excluded, "renameByName": renamed, "indexByName": index}}
}

// ranking is a top-N list: label columns, then the value as inline bars, largest first.
func ranking(title, desc, expr string, labels ...column) *panel {
	const value = "Assets"
	columns := append(slices.Clone(labels), col("Value", value))
	return table(title, desc, frame(expr)).
		transform(organize([]string{"Time"}, columns...)).
		override(value, barCell("")...).
		sortBy(value)
}

// assetTypes names and colors Immich's asset types the same way on every panel.
func assetTypes(p *panel) *panel {
	return p.override("IMAGE", prop("displayName", "Photos"), prop("color", fixed(blue))).
		override("VIDEO", prop("displayName", "Videos"), prop("color", fixed(purple)))
}

// canvas draws elements over the panel's data. An element shows a field by its display name: the legend of a
// time series, or the column of a table.
func canvas(title, desc string, elements []obj, targets ...target) *panel {
	p := newPanel("canvas", title, desc, targets...)
	p.set("unit", "none").set("color", obj{"mode": "thresholds"}).set("thresholds", steps(text))
	p.Options = obj{
		"inlineEditing": false, "showAdvancedTypes": true, "panZoom": false, "infinitePan": false,
		"root": obj{
			"type": "frame", "name": "Root", "elements": elements, "placement": obj{},
			"constraint": obj{"horizontal": "left", "vertical": "top"},
			"background": obj{"color": obj{"fixed": "transparent"}}, "border": obj{"color": obj{"fixed": "transparent"}},
		},
	}
	return p
}

// pinned places a canvas element in pixels: from the panel's left edge, and its center up from the panel's middle.
func pinned(left, up, width, height float64) obj {
	return obj{
		"constraint": obj{"horizontal": "left", "vertical": "center"},
		"placement":  obj{"left": left, "top": up, "width": width, "height": height},
	}
}

// scaled places a canvas element in percent of the panel, so it stretches with the panel.
func scaled(left, top, width, height float64) obj {
	return obj{
		"constraint": obj{"horizontal": "scale", "vertical": "scale"},
		"placement":  obj{"left": left, "top": top, "right": 100 - left - width, "bottom": 100 - top - height},
	}
}

func element(kind, name string, at, config obj) obj {
	e := obj{
		"type": kind, "name": name, "config": config,
		"background": obj{"color": obj{"fixed": "transparent"}},
		"border":     obj{"color": obj{"fixed": "transparent"}, "width": 0},
	}
	maps.Copy(e, at)
	return e
}

// fieldColor is the color a field's thresholds and mappings give its latest value.
func fieldColor(field, fallback string) obj { return obj{"fixed": fallback, "field": field} }

// label is fixed canvas text in the theme's text color.
func label(name, s string, size int, align string, at obj) obj {
	return element("text", name, at, obj{"text": obj{"mode": "fixed", "fixed": s}, "size": size, "align": align, "valign": "middle"})
}

// reading is a field's latest value as canvas text, or fallback while the field is missing, in the theme's text
// color unless colorField names a field to take the color from.
func reading(name, field, colorField, fallback string, size int, align string, at obj) obj {
	config := obj{"text": obj{"mode": "field", "field": field, "fixed": fallback}, "size": size, "align": align, "valign": "middle"}
	if colorField != "" {
		config["color"] = fieldColor(colorField, text)
	}
	return element("text", name, at, config)
}

// dot is a status light in the color of a field.
func dot(name, field string, at obj) obj {
	e := element("ellipse", name, at, obj{"text": obj{"mode": "fixed", "fixed": ""}, "color": obj{"fixed": "transparent"}, "size": 8})
	e["background"] = obj{"color": fieldColor(field, text)}
	return e
}

func image(name, uri string, at obj) obj {
	e := element("rectangle", name, at, obj{"text": obj{"mode": "fixed", "fixed": ""}})
	e["background"] = obj{"color": obj{"fixed": "transparent"}, "image": obj{"mode": "fixed", "fixed": uri}, "size": "contain"}
	return e
}

// link connects the right edge of its element to the left edge of the element named target.
func link(target string, color obj, width float64) obj {
	return obj{
		"targetName": target, "source": obj{"x": 1, "y": 0}, "target": obj{"x": -1, "y": 0},
		"path": "straight", "vertices": []obj{}, "color": color,
		"size": obj{"fixed": width, "min": 1, "max": 10}, "radius": obj{"fixed": 0, "min": 0, "max": 200},
	}
}

type cell struct {
	w int
	p *panel
}

func span(w int, p *panel) cell { return cell{w: w, p: p} }

// layout places panels on Grafana's 24-column grid, one line at a time, under rows that may start collapsed.
type layout struct {
	items []any
	open  *row
	y     int
	rowY  int
	id    int
}

func (l *layout) nextID() int {
	l.id++
	return l.id
}

func (l *layout) section(title string, collapsed bool) {
	r := &row{ID: l.nextID(), Type: "row", Title: title, Collapsed: collapsed, GridPos: gridPos{H: 1, W: gridWidth, Y: l.y}}
	l.items = append(l.items, r)
	l.y++
	l.open = nil
	if collapsed {
		l.open, l.rowY = r, l.y
	}
}

func (l *layout) line(h int, cells ...cell) {
	y := l.y
	if l.open != nil {
		y = l.rowY
	}
	x := 0
	for _, c := range cells {
		c.p.ID = l.nextID()
		c.p.GridPos = gridPos{H: h, W: c.w, X: x, Y: y}
		if l.open != nil {
			l.open.Panels = append(l.open.Panels, c.p)
		} else {
			l.items = append(l.items, c.p)
		}
		x += c.w
	}
	if x != gridWidth {
		panic(fmt.Sprintf("line at y=%d spans %d columns, not %d", y, x, gridWidth))
	}
	if l.open != nil {
		l.rowY += h
	} else {
		l.y += h
	}
}
