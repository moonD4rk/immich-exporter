package grafana

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/prometheus/common/model"
)

// defaultScrapeInterval is Grafana's default datasource scrape interval, the floor of $__interval.
const defaultScrapeInterval = 15 * time.Second

// builtinVars substitutes Grafana's built-in variables at a 5m range for a panel whose $__interval resolves to
// interval. $__rate_interval is Grafana's max($__interval + scrape interval, 4 * scrape interval).
func builtinVars(interval time.Duration) *strings.Replacer {
	rate := model.Duration(max(interval+defaultScrapeInterval, 4*defaultScrapeInterval)).String()
	step, ms := model.Duration(interval).String(), strconv.FormatInt(interval.Milliseconds(), 10)
	return strings.NewReplacer(
		"${__rate_interval}", rate, "$__rate_interval", rate,
		"${__interval_ms}", ms, "$__interval_ms", ms,
		"${__interval}", step, "$__interval", step,
		"${__range_ms}", "300000", "$__range_ms", "300000",
		"${__range_s}", "300", "$__range_s", "300",
		"${__range}", "5m", "$__range", "5m",
	)
}

// Grafana's values for its built-in variables at default datasource settings (15s scrape interval, 5m range).
var grafanaVars = builtinVars(defaultScrapeInterval)

var (
	quotedString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	metricName   = regexp.MustCompile(`\bimmich_[a-z0-9_]+`)
	rangeWindow  = regexp.MustCompile(`\[(\d[0-9a-z]*)(?::[^\]]*)?\]`)
)

type datasourceRef struct {
	Type string `json:"type"`
}

type dashboardTarget struct {
	Datasource   *datasourceRef `json:"datasource"`
	RefID        string         `json:"refId"`
	Expr         string         `json:"expr"`
	LegendFormat string         `json:"legendFormat"`
	Format       string         `json:"format"`
	Instant      bool           `json:"instant"`
	Range        bool           `json:"range"`
}

type dashboardPanel struct {
	ID         int               `json:"id"`
	Title      string            `json:"title"`
	Interval   string            `json:"interval"`
	Datasource *datasourceRef    `json:"datasource"`
	Targets    []dashboardTarget `json:"targets"`
	Panels     []dashboardPanel  `json:"panels"`
}

type dashboard struct {
	Panels     []dashboardPanel `json:"panels"`
	Templating struct {
		List []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"list"`
	} `json:"templating"`
}

type query struct {
	name         string
	datasource   string
	expr         string
	template     string        // expr before substitution
	minInterval  time.Duration // the panel's minimum interval, which raises $__interval
	legendFormat string
	format       string
	rangeQuery   bool
}

// showsMetricName reports whether Grafana renders __name__ for this target: tables list
// every label, and an unset legend falls back to the full series name.
func (q *query) showsMetricName() bool {
	return q.format == "table" || q.legendFormat == "" || q.legendFormat == "__auto" ||
		strings.Contains(q.legendFormat, "__name__")
}

// loadQueries flattens every panel target, with Grafana's built-in variables substituted.
func loadQueries(t *testing.T) (*dashboard, []query) {
	t.Helper()
	raw, err := os.ReadFile("dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var d dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse dashboard.json: %v", err)
	}
	var qs []query
	var walk func(panels []dashboardPanel)
	walk = func(panels []dashboardPanel) {
		for i := range panels {
			p := &panels[i]
			var minInterval model.Duration
			if p.Interval != "" {
				d, err := model.ParseDuration(p.Interval)
				if err != nil {
					t.Fatalf("%d %s: interval: %v", p.ID, p.Title, err)
				}
				minInterval = d
			}
			for j := range p.Targets {
				tg := &p.Targets[j]
				ds := p.Datasource
				if tg.Datasource != nil {
					ds = tg.Datasource
				}
				q := query{
					name:         fmt.Sprintf("%d %s %s", p.ID, p.Title, tg.RefID),
					expr:         grafanaVars.Replace(tg.Expr),
					template:     tg.Expr,
					minInterval:  time.Duration(minInterval),
					legendFormat: tg.LegendFormat,
					format:       tg.Format,
					rangeQuery:   tg.Range || !tg.Instant,
				}
				if ds != nil {
					q.datasource = ds.Type
				}
				qs = append(qs, q)
			}
			walk(p.Panels)
		}
	}
	walk(d.Panels)
	if len(qs) == 0 {
		t.Fatal("dashboard.json has no queries")
	}
	return &d, qs
}

func withoutStrings(expr string) string { return quotedString.ReplaceAllString(expr, `""`) }

func TestDashboardMetricsExistInGolden(t *testing.T) {
	_, qs := loadQueries(t)
	raw, err := os.ReadFile("../internal/exporter/testdata/metrics.golden")
	if err != nil {
		t.Fatal(err)
	}
	exported := map[string]bool{}
	for line := range strings.Lines(string(raw)) {
		if f := strings.Fields(line); len(f) >= 3 && f[0] == "#" && f[1] == "TYPE" {
			exported[f[2]] = true
		}
	}
	for i := range qs {
		for _, m := range metricName.FindAllString(withoutStrings(qs[i].expr), -1) {
			if !exported[m] {
				t.Errorf("%s: %s is not in metrics.golden (renamed or removed, or missing from the poll mock)", qs[i].name, m)
			}
		}
	}
}

// windowProblems flags range windows too short to hold two samples on Prometheus,
// which then returns nothing where VictoriaMetrics still looks back past the window.
func windowProblems(expr string, minWindow time.Duration) []string {
	var problems []string
	for _, m := range rangeWindow.FindAllStringSubmatch(withoutStrings(expr), -1) {
		d, err := model.ParseDuration(m[1])
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("window [%s]: %v", m[1], err))
		case time.Duration(d) < minWindow:
			problems = append(problems, fmt.Sprintf("window [%s] is shorter than %s", m[1], model.Duration(minWindow)))
		}
	}
	return problems
}

func TestDashboardRangeWindowsCoverScrapeInterval(t *testing.T) {
	_, qs := loadQueries(t)
	raw, err := os.ReadFile("../deploy/prometheus.yml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Global struct {
			ScrapeInterval model.Duration `yaml:"scrape_interval"`
		} `yaml:"global"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Global.ScrapeInterval == 0 {
		t.Fatal("deploy/prometheus.yml sets no global scrape_interval")
	}
	// Four intervals, like Grafana's $__rate_interval, so one missed scrape still leaves two samples.
	minWindow := 4 * time.Duration(cfg.Global.ScrapeInterval)
	for i := range qs {
		expr := builtinVars(max(defaultScrapeInterval, qs[i].minInterval)).Replace(qs[i].template)
		for _, p := range windowProblems(expr, minWindow) {
			t.Errorf("%s: %s (4x the documented %s scrape interval)", qs[i].name, p, cfg.Global.ScrapeInterval)
		}
	}
}

func TestWindowProblems(t *testing.T) {
	cases := []struct {
		expr string
		want int
	}{
		{`rate(immich_exporter_scrape_errors_total[5m])`, 0},
		{`increase(immich_exporter_scrape_errors_total[1h])`, 0},
		{`max_over_time(immich_people[10m:1m])`, 0},
		{`rate(immich_exporter_scrape_errors_total[1m])`, 1},
		{grafanaVars.Replace(`rate(immich_exporter_scrape_errors_total[$__rate_interval])`), 1},
		{`immich_albums{album="[1s]"}`, 0},
	}
	for _, c := range cases {
		if got := windowProblems(c.expr, 4*time.Minute); len(got) != c.want {
			t.Errorf("windowProblems(%q) = %v, want %d problem(s)", c.expr, got, c.want)
		}
	}
}

// An input the dashboard never references still gets a picker on import, and the choice is silently ignored.
func TestDashboardInputsAreReferenced(t *testing.T) {
	raw, err := os.ReadFile("dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Inputs []struct {
			Name string `json:"name"`
		} `json:"__inputs"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	for _, in := range d.Inputs {
		if !strings.Contains(string(raw), "${"+in.Name+"}") {
			t.Errorf("__inputs %s is never referenced; set the datasource variable's current value to ${%s}", in.Name, in.Name)
		}
	}
}

func TestDashboardUsesOnlyCheckedFeatures(t *testing.T) {
	d, qs := loadQueries(t)
	for _, v := range d.Templating.List {
		if v.Type == "query" || v.Type == "adhoc" {
			t.Errorf("variable %q (%s) is not exercised by backends_test.go; extend it before adding one", v.Name, v.Type)
		}
	}
	for i := range qs {
		if qs[i].datasource != "" && qs[i].datasource != "prometheus" {
			t.Errorf("%s: datasource type %q is not covered by backends_test.go", qs[i].name, qs[i].datasource)
		}
		if strings.TrimSpace(qs[i].expr) == "" {
			t.Errorf("%s: empty query", qs[i].name)
		}
	}
}

// immichQueues are the queues Immich reports: QueueName in server/src/enum.ts of immich-app/immich.
var immichQueues = []string{
	"thumbnailGeneration", "metadataExtraction", "videoConversion", "faceDetection", "facialRecognition",
	"smartSearch", "duplicateDetection", "backgroundTask", "storageTemplateMigration", "migration", "search",
	"sidecar", "library", "notifications", "backupDatabase", "ocr", "workflow", "integrityCheck", "editor",
}

// A pipeline box finds its queue and the boxes it feeds by name, so a typo shows a dash or drops a link silently.
func TestPipelineStages(t *testing.T) {
	boxes := map[string]bool{}
	for _, s := range pipelineStages {
		boxes[s.name] = true
	}
	for _, s := range pipelineStages {
		if s.field != "uploads" && !slices.Contains(immichQueues, s.field) {
			t.Errorf("pipeline box %q shows queue %q, which Immich doesn't report", s.name, s.field)
		}
		for _, next := range s.feeds {
			if !boxes[next] {
				t.Errorf("pipeline box %q feeds %q, which has no box", s.name, next)
			}
		}
	}
}

// dashboard.json is generated from dashboard.go, so a hand edit would be lost on the next go generate.
func TestDashboardIsGenerated(t *testing.T) {
	want, err := JSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("dashboard.json differs from dashboard.go: edit dashboard.go, then run go generate ./grafana")
	}
}
