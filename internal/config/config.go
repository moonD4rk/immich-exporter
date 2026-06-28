// Package config defines the exporter's configuration and loads it from an
// optional YAML file. Command-line flags and environment variables (wired up in
// main) overlay these values; the effective precedence is
// flags > env > YAML > built-in defaults.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/prometheus/common/model"
)

// Config is the full exporter configuration. Durations use model.Duration so a
// YAML file can write human-friendly values like "2m" / "15m" / "20s".
type Config struct {
	Immich  Immich  `yaml:"immich"`
	Web     Web     `yaml:"web"`
	Scrape  Scrape  `yaml:"scrape"`
	Collect Collect `yaml:"collect"`
	Limits  Limits  `yaml:"limits"`
}

// Immich locates the server and supplies the API token. Prefer the env var
// IMMICH_API_TOKEN or token_file over an inline token (a YAML file is easy to
// commit to git by accident).
type Immich struct {
	BaseURL   string `yaml:"base_url"`
	Host      string `yaml:"host"`
	Port      string `yaml:"port"`
	Token     string `yaml:"token"`
	TokenFile string `yaml:"token_file"`
}

// Web controls the metrics HTTP server.
type Web struct {
	ListenAddress string `yaml:"listen_address"`
	TelemetryPath string `yaml:"telemetry_path"`
}

// Scrape controls poll cadence and per-request timeout.
type Scrape struct {
	Interval          model.Duration `yaml:"interval"`
	BreakdownInterval model.Duration `yaml:"breakdown_interval"`
	RequestTimeout    model.Duration `yaml:"request_timeout"`
}

// Collect toggles optional collector groups.
type Collect struct {
	Camera  bool `yaml:"camera"`
	Geo     bool `yaml:"geo"`
	Ratings bool `yaml:"ratings"`
	People  bool `yaml:"people"`
	Heavy   bool `yaml:"heavy"`
}

// Limits bounds the high-cardinality fan-out breakdowns.
type Limits struct {
	TopN              int `yaml:"topn"`
	FanoutLimit       int `yaml:"fanout_limit"`
	FanoutConcurrency int `yaml:"fanout_concurrency"`
}

// Default returns the built-in configuration before any YAML/env/flag overlay.
func Default() Config {
	return Config{
		Immich: Immich{Host: "localhost", Port: "2283"},
		Web:    Web{ListenAddress: ":8000", TelemetryPath: "/metrics"},
		Scrape: Scrape{
			Interval:          model.Duration(2 * time.Minute),
			BreakdownInterval: model.Duration(15 * time.Minute),
			RequestTimeout:    model.Duration(20 * time.Second),
		},
		Collect: Collect{Camera: true, Geo: true, Ratings: true, People: false, Heavy: true},
		Limits:  Limits{TopN: 25, FanoutLimit: 200, FanoutConcurrency: 8},
	}
}

// LoadYAML overlays the YAML file at path onto c, leaving fields the file omits
// untouched (so they keep their prior value, e.g. a built-in default). Unknown
// fields are rejected so a typo fails loudly instead of being silently ignored.
func (c *Config) LoadYAML(path string) error {
	b, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied configuration.
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}
	if err := yaml.UnmarshalWithOptions(b, c, yaml.Strict()); err != nil {
		return fmt.Errorf("parse config file %s:\n%s", path, yaml.FormatError(err, false, true))
	}
	return nil
}
