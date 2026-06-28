// Command immich-exporter exposes Immich photo-library content metrics to
// Prometheus: assets (by type/year/rating/state), cameras & EXIF, geographic
// distribution by country, people & faces, per-user storage & quota, albums &
// sharing, tags/memories/duplicates/stacks/libraries, job-queue depths, and
// server version/storage/health.
//
// It complements Immich's built-in OpenTelemetry (which covers performance
// only). All data is read from the public Immich REST API via the x-api-key
// header; the database is never touched. A background poller stores an atomic
// snapshot and a custom collector emits it fresh on every scrape.
//
// Configuration is layered: built-in defaults < an optional YAML file
// (--config.file) < environment variables < command-line flags.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promslog"
	promslogflag "github.com/prometheus/common/promslog/flag"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"

	"github.com/moonD4rk/immich-exporter/internal/config"
	"github.com/moonD4rk/immich-exporter/internal/exporter"
	"github.com/moonD4rk/immich-exporter/internal/immich"
)

var version = "dev" // overridden at build time via -ldflags "-X main.version=..."

// appFlags holds the resolved command-line flag values.
type appFlags struct {
	web               *web.FlagConfig
	telemetryPath     *string
	baseURL           *string
	host, port        *string
	interval          *time.Duration
	breakdownInterval *time.Duration
	requestTimeout    *time.Duration
	camera, geo       *bool
	ratings           *bool
	people, heavy     *bool
	topN              *int
	fanoutLimit       *int
	fanoutConcurrency *int
}

func main() {
	cfg := config.Default()
	if path := configFilePath(); path != "" {
		if err := cfg.LoadYAML(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	app := kingpin.New("immich-exporter", "Prometheus exporter for Immich photo-library content.")
	app.Flag("config.file", "Path to a YAML config file (overlaid by env vars and flags).").Envar("IMMICH_CONFIG_FILE").String()
	flags := registerFlags(app, &cfg)
	promslogConfig := &promslog.Config{}
	promslogflag.AddFlags(app, promslogConfig)
	app.Version(version)
	app.HelpFlag.Short('h')
	kingpin.MustParse(app.Parse(os.Args[1:]))

	logger := promslog.New(promslogConfig)
	slog.SetDefault(logger)

	token, err := resolveToken(&cfg.Immich)
	if err != nil {
		logger.Error("API token", "err", err)
		os.Exit(1)
	}

	base := *flags.baseURL
	if base == "" {
		// Immich serves its REST API under /api (the web SPA lives at the root).
		base = fmt.Sprintf("http://%s:%s/api", *flags.host, *flags.port)
	}
	if *flags.fanoutConcurrency < 1 {
		*flags.fanoutConcurrency = 1
	}

	c := immich.New(base, token, &http.Client{Timeout: *flags.requestTimeout})
	exp := exporter.New(c, version, exporterConfig(flags))

	reg := prometheus.NewRegistry()
	reg.MustRegister(exp, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go exp.Run(ctx)

	srv := &http.Server{Handler: buildMux(reg, *flags.telemetryPath, logger), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		//nolint:contextcheck // graceful shutdown needs a fresh context; the parent is already canceled.
		_ = srv.Shutdown(shutCtx)
	}()

	logger.Info("starting immich-exporter", "version", version, "immich", base,
		"scrape_interval", flags.interval.String(), "breakdown_interval", flags.breakdownInterval.String())
	if err := web.ListenAndServe(srv, flags.web, logger); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("web server", "err", err)
		os.Exit(1)
	}
}

// registerFlags defines every flag, seeding each default from cfg (which has
// already merged YAML over the built-in defaults) and an Envar for env override.
// Precedence ends up flags > env > YAML > defaults. The API token is never a
// flag — it would otherwise be visible in the process list.
func registerFlags(app *kingpin.Application, cfg *config.Config) *appFlags {
	listenDefault := cfg.Web.ListenAddress
	if p := os.Getenv("EXPORTER_PORT"); p != "" { // legacy env compatibility
		listenDefault = ":" + p
	}
	return &appFlags{
		web:               webflag.AddFlags(app, listenDefault),
		telemetryPath:     app.Flag("web.telemetry-path", "Path under which to expose metrics.").Default(cfg.Web.TelemetryPath).String(),
		baseURL:           app.Flag("immich.base-url", "Full Immich API base URL incl. /api (overrides host/port).").Default(cfg.Immich.BaseURL).Envar("IMMICH_BASE_URL").String(),
		host:              app.Flag("immich.host", "Immich server host.").Default(cfg.Immich.Host).Envar("IMMICH_HOST").String(),
		port:              app.Flag("immich.port", "Immich server port.").Default(cfg.Immich.Port).Envar("IMMICH_PORT").String(),
		interval:          app.Flag("scrape-interval", "Cheap-metric poll interval.").Default(durDefault("SCRAPE_INTERVAL_SECONDS", cfg.Scrape.Interval)).Envar("SCRAPE_INTERVAL").Duration(),
		breakdownInterval: app.Flag("breakdown-interval", "Expensive fan-out refresh interval.").Default(durDefault("BREAKDOWN_INTERVAL_SECONDS", cfg.Scrape.BreakdownInterval)).Envar("BREAKDOWN_INTERVAL").Duration(),
		requestTimeout:    app.Flag("request-timeout", "Per-request HTTP timeout.").Default(durDefault("REQUEST_TIMEOUT_SECONDS", cfg.Scrape.RequestTimeout)).Envar("REQUEST_TIMEOUT").Duration(),
		camera:            app.Flag("collect.camera", "Collect per camera make/model/lens breakdowns.").Default(strconv.FormatBool(cfg.Collect.Camera)).Envar("COLLECT_CAMERA").Bool(),
		geo:               app.Flag("collect.geo", "Collect geographic distribution.").Default(strconv.FormatBool(cfg.Collect.Geo)).Envar("COLLECT_GEO").Bool(),
		ratings:           app.Flag("collect.ratings", "Collect star-rating breakdown.").Default(strconv.FormatBool(cfg.Collect.Ratings)).Envar("COLLECT_RATINGS").Bool(),
		people:            app.Flag("collect.people", "Collect per-person asset stats (one API call per person).").Default(strconv.FormatBool(cfg.Collect.People)).Envar("COLLECT_PEOPLE").Bool(),
		heavy:             app.Flag("collect.heavy", "Collect duplicates & stacks.").Default(strconv.FormatBool(cfg.Collect.Heavy)).Envar("COLLECT_HEAVY").Bool(),
		topN:              app.Flag("topn", "Series cap for model/lens/person/album breakdowns.").Default(strconv.Itoa(cfg.Limits.TopN)).Envar("TOPN").Int(),
		fanoutLimit:       app.Flag("fanout.limit", "Skip a breakdown with more distinct values than this.").Default(strconv.Itoa(cfg.Limits.FanoutLimit)).Envar("FANOUT_LIMIT").Int(),
		fanoutConcurrency: app.Flag("fanout.concurrency", "Concurrent fan-out requests.").Default(strconv.Itoa(cfg.Limits.FanoutConcurrency)).Envar("FANOUT_CONCURRENCY").Int(),
	}
}

func exporterConfig(f *appFlags) exporter.Config {
	return exporter.Config{
		Interval:          *f.interval,
		BreakdownInterval: *f.breakdownInterval,
		CollectCamera:     *f.camera,
		CollectGeo:        *f.geo,
		CollectRatings:    *f.ratings,
		CollectPeople:     *f.people,
		CollectHeavy:      *f.heavy,
		TopN:              *f.topN,
		FanoutLimit:       *f.fanoutLimit,
		FanoutConcurrency: *f.fanoutConcurrency,
	}
}

func buildMux(reg *prometheus.Registry, metricsPath string, logger *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(metricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	if metricsPath != "/" {
		landing, err := web.NewLandingPage(web.LandingConfig{
			Name:        "Immich Exporter",
			Description: "Prometheus exporter for Immich photo-library content.",
			Version:     version,
			Links: []web.LandingLinks{
				{Address: metricsPath, Text: "Metrics"},
				{Address: "/healthz", Text: "Health"},
			},
		})
		if err != nil {
			logger.Error("landing page", "err", err)
		} else {
			mux.Handle("/", landing)
		}
	}
	return mux
}

// configFilePath finds --config.file (or IMMICH_CONFIG_FILE) before the main
// flag parse, so the YAML can seed the other flags' defaults.
func configFilePath() string {
	args := os.Args[1:]
	for i, a := range args {
		if a == "--config.file" && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--config.file="); ok {
			return v
		}
	}
	return os.Getenv("IMMICH_CONFIG_FILE")
}

// resolveToken reads the API token with precedence env > token_file > inline
// YAML token.
func resolveToken(im *config.Immich) (string, error) {
	if v := os.Getenv("IMMICH_API_TOKEN"); v != "" {
		return v, nil
	}
	if im.TokenFile != "" {
		b, err := os.ReadFile(im.TokenFile)
		if err != nil {
			return "", fmt.Errorf("read token_file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if im.Token != "" {
		return im.Token, nil
	}
	return "", errors.New("no API token: set IMMICH_API_TOKEN, immich.token_file, or immich.token")
}

// durDefault seeds a duration flag's default from the legacy *_SECONDS env var
// (plain integer seconds) when set, else from the merged YAML/built-in value.
func durDefault(legacyEnv string, merged model.Duration) string {
	if v := os.Getenv(legacyEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return (time.Duration(n) * time.Second).String()
		}
	}
	return time.Duration(merged).String()
}
