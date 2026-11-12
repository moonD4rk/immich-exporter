package exporter

import (
	"os"
	"strings"
	"testing"
)

// docs/metrics.md lists the metrics from their descriptors, so a hand edit or a new metric shows up here.
func TestMetricsReferenceIsGenerated(t *testing.T) {
	doc, err := os.ReadFile("../../docs/metrics.md")
	if err != nil {
		t.Fatal(err)
	}
	want, err := UpdateReference(string(doc))
	if err != nil {
		t.Fatal(err)
	}
	if string(doc) != want {
		t.Error("docs/metrics.md differs from the metric descriptors: run go generate ./internal/exporter")
	}
}

// A metric in a group that groups doesn't list would be missing from the reference without a word.
func TestReferenceListsEveryMetric(t *testing.T) {
	ref := Reference()
	for _, m := range metrics {
		if !strings.Contains(ref, "| `"+m.name+"` ") {
			t.Errorf("%s (group %q) is not in the reference", m.name, m.group)
		}
		if strings.TrimSpace(m.help) == "" || !strings.HasSuffix(m.help, ".") {
			t.Errorf("%s: help %q should be a sentence", m.name, m.help)
		}
	}
	if len(metrics) != len(allDescs) {
		t.Errorf("%d documented metrics for %d descriptors", len(metrics), len(allDescs))
	}
}
