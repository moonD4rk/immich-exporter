package grafana

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

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
