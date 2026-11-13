package grafana

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

var measuresChange = regexp.MustCompile(`\b(?:delta|idelta|increase|rate|irate|deriv|predict_linear|changes|resets)\(|\boffset\b`)

var forecastsOneSeries = regexp.MustCompile(`\b(?:deriv|predict_linear)\(\s*[a-zA-Z_:][a-zA-Z0-9_:]*\s*(?:\{[^}]*\})?\s*\[`)

type alertRule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

func loadAlertRules(t *testing.T) []alertRule {
	t.Helper()
	raw, err := os.ReadFile("../deploy/alerts.yml")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Groups []struct {
			Name  string      `yaml:"name"`
			Rules []alertRule `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.UnmarshalWithOptions(raw, &file, yaml.Strict()); err != nil {
		t.Fatalf("parse deploy/alerts.yml: %v", err)
	}
	var rules []alertRule
	for _, g := range file.Groups {
		rules = append(rules, g.Rules...)
	}
	if len(rules) == 0 {
		t.Fatal("deploy/alerts.yml has no rules")
	}
	return rules
}

// An alert on a renamed or removed metric never fires, which looks exactly like a healthy Immich.
func TestAlertRulesUseExportedMetrics(t *testing.T) {
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
	seen := map[string]bool{}
	for _, r := range loadAlertRules(t) {
		if seen[r.Alert] {
			t.Errorf("alert %s is defined twice", r.Alert)
		}
		seen[r.Alert] = true
		for _, m := range metricName.FindAllString(withoutStrings(r.Expr), -1) {
			if !exported[m] {
				t.Errorf("%s: %s is not in metrics.golden", r.Alert, m)
			}
		}
		if s := r.Labels["severity"]; s != "critical" && s != "warning" && s != "info" {
			t.Errorf("%s: severity %q, want critical, warning or info", r.Alert, s)
		}
		if r.Annotations["summary"] == "" || r.Annotations["description"] == "" {
			t.Errorf("%s: needs a summary and a description", r.Alert)
		}
	}
}

// Rule expressions evaluate as instant queries on both backends; they usually return nothing on the golden
// data, so only errors count, such as MetricsQL that Prometheus rejects.
func TestAlertRulesEvaluateOnBackends(t *testing.T) {
	bs := liveBackends(t)
	at := time.Now()
	for _, r := range loadAlertRules(t) {
		t.Run(r.Alert, func(t *testing.T) {
			for _, b := range bs {
				if _, err := b.instant(t.Context(), r.Expr, at); err != nil {
					t.Errorf("%s: %v", b.name, err)
				}
			}
		})
	}
}

// A rule that measures change must stay silent while nothing changes. The backends hold minutes of constant
// golden data, so every series starts inside any longer window: the shape a restarted exporter leaves behind,
// whose new series VictoriaMetrics' delta() counts as growth from zero.
func TestChangeAlertsStaySilentOnSteadyData(t *testing.T) {
	bs := liveBackends(t)
	at := time.Now()
	for _, r := range loadAlertRules(t) {
		if !measuresChange.MatchString(withoutStrings(r.Expr)) {
			continue
		}
		t.Run(r.Alert, func(t *testing.T) {
			for _, b := range bs {
				got, err := b.instant(t.Context(), r.Expr, at)
				switch {
				case err != nil:
					t.Errorf("%s: %v", b.name, err)
				case len(got) > 0:
					t.Errorf("%s: fires on unchanging data: %v", b.name, got)
				}
			}
		})
	}
}

// A restarted exporter is a new series in Kubernetes: a forecast over it alone reads minutes as the whole window.
func TestForecastsReadAggregatedSeries(t *testing.T) {
	const fix = "forecasts from one series; aggregate first, as in deriv(max(m)[7d:1h])"
	for _, r := range loadAlertRules(t) {
		if forecastsOneSeries.MatchString(withoutStrings(r.Expr)) {
			t.Errorf("alert %s %s", r.Alert, fix)
		}
	}
	_, qs := loadQueries(t)
	for i := range qs {
		if forecastsOneSeries.MatchString(withoutStrings(qs[i].expr)) {
			t.Errorf("%s %s", qs[i].name, fix)
		}
	}
}

func TestForecastsOneSeries(t *testing.T) {
	cases := []struct {
		expr string
		want bool
	}{
		{`predict_linear(immich_storage_available_bytes[7d], 30 * 24 * 3600) < 0`, true},
		{`max(deriv(immich_storage_used_bytes{instance=""}[7d]))`, true},
		{`predict_linear(max(immich_storage_available_bytes)[7d:1h], 30 * 24 * 3600) < 0`, false},
		{`deriv(max(immich_storage_used_bytes)[7d:1h]) > 0`, false},
		{`max by (queue) (immich_job_queue offset 1h)`, false},
	}
	for _, c := range cases {
		if got := forecastsOneSeries.MatchString(c.expr); got != c.want {
			t.Errorf("forecastsOneSeries(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
}
