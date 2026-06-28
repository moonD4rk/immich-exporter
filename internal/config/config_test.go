package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadYAMLOverlaysDefaults(t *testing.T) {
	c := Default()
	path := writeTemp(t, `
immich:
  base_url: http://immich:2283/api
scrape:
  interval: 5m
collect:
  people: true
  heavy: false
limits:
  topn: 50
`)
	if err := c.LoadYAML(path); err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}

	// Fields present in YAML are overridden.
	if c.Immich.BaseURL != "http://immich:2283/api" {
		t.Errorf("base_url = %q", c.Immich.BaseURL)
	}
	if got := time.Duration(c.Scrape.Interval); got != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", got)
	}
	if !c.Collect.People {
		t.Error("collect.people should be true")
	}
	if c.Collect.Heavy {
		t.Error("collect.heavy should be false")
	}
	if c.Limits.TopN != 50 {
		t.Errorf("topn = %d, want 50", c.Limits.TopN)
	}

	// Fields the YAML omits keep their defaults.
	if c.Immich.Host != "localhost" || c.Immich.Port != "2283" {
		t.Errorf("host/port = %q/%q, want localhost/2283", c.Immich.Host, c.Immich.Port)
	}
	if c.Web.ListenAddress != ":8000" {
		t.Errorf("listen = %q, want :8000", c.Web.ListenAddress)
	}
	if got := time.Duration(c.Scrape.BreakdownInterval); got != 15*time.Minute {
		t.Errorf("breakdown_interval = %v, want 15m (default)", got)
	}
	if !c.Collect.Camera || !c.Collect.Geo || !c.Collect.Ratings {
		t.Error("camera/geo/ratings should keep default true")
	}
	if c.Limits.FanoutLimit != 200 || c.Limits.FanoutConcurrency != 8 {
		t.Errorf("fanout defaults lost: %+v", c.Limits)
	}
}

func TestLoadYAMLRejectsUnknownField(t *testing.T) {
	c := Default()
	path := writeTemp(t, "scrape:\n  interval: 5m\nbogus_top_level: 1\n")
	if err := c.LoadYAML(path); err == nil {
		t.Fatal("expected an error for an unknown field, got nil")
	}
}

func TestLoadYAMLInvalidDuration(t *testing.T) {
	c := Default()
	path := writeTemp(t, "scrape:\n  interval: not-a-duration\n")
	if err := c.LoadYAML(path); err == nil {
		t.Fatal("expected an error for an invalid duration")
	}
}
