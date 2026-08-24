package exporter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestGoldenMetrics locks the exact /metrics text rendered from a full mock
// poll, so internal refactors cannot silently change the exported surface.
// After an INTENTIONAL metrics change, regenerate with:
//
//	UPDATE_GOLDEN=1 go test ./internal/exporter -run TestGoldenMetrics
func TestGoldenMetrics(t *testing.T) {
	srv := immichMock(t, true)
	p, snap := testPoller(srv)
	p.poll(context.Background())

	s := snap.Load()
	if s == nil {
		t.Fatal("nil snapshot")
	}
	// Pin the two wall-clock-dependent values.
	s.health.scrapeDurationSeconds = 0.42
	s.health.lastSuccessUnix = 1.7e9

	reg := prometheus.NewRegistry()
	reg.MustRegister(&collector{snap: snap, scrapeErrors: p.scrapeErrors, version: "golden", goVersion: "go-golden"})
	got := renderText(t, reg)

	golden := filepath.Join("testdata", "metrics.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wantBytes, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden file (create it with UPDATE_GOLDEN=1): %v", err)
	}
	want := string(wantBytes)
	if got == want {
		return
	}
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
	lines := max(len(gotLines), len(wantLines))
	var diffs int
	for i := 0; i < lines && diffs < 20; i++ {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			diffs++
			t.Errorf("line %d:\n  got:  %s\n  want: %s", i+1, g, w)
		}
	}
	t.Errorf("metrics output differs from %s (%d+ lines); regenerate with UPDATE_GOLDEN=1 if intentional", golden, diffs)
}
