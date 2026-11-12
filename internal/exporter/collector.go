package exporter

import (
	"context"
	"log/slog"
	"runtime"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/moond4rk/immich-exporter/internal/immich"
)

const ns = "immich"

// opt is a float64 that knows whether it was actually collected. Absent values
// are simply not emitted, preserving "absent vs zero" semantics for endpoints
// that are admin-gated or missing on older Immich versions.
type opt struct {
	v  float64
	ok bool
}

func set(v float64) opt { return opt{v, true} }

// labeledVal is a single labeled gauge value (id + display name + value),
// used for top-N breakdowns (people, albums, libraries) where a stable id label
// keeps series alive across renames.
type labeledVal struct {
	id, name string
	value    float64
}

func optional(v *float64) opt {
	if v == nil {
		return opt{}
	}
	return set(*v)
}

// collectorStat is how one collector fared on its latest run.
type collectorStat struct {
	ok      bool
	refused bool // the API key may not use it or the server lacks it, so a retry would fare no better
	seconds float64
}

type healthSnap struct {
	serverUp, scrapeSuccess, scrapeDurationSeconds, lastSuccessUnix float64
	keyIsAdmin                                                      opt
	collectors                                                      map[string]collectorStat
}

// snapshot is the complete set of values from one poll cycle, grouped by the
// same domains the emit functions use. The collector emits it verbatim on
// every Prometheus scrape, so a label combination that disappears upstream
// simply stops being emitted (no stale series).
type snapshot struct {
	health   healthSnap
	server   serverSnap
	ops      opsSnap
	assets   assetsSnap
	metadata metadataSnap
	cameras  camerasSnap
	geo      geoSnap
	people   peopleSnap
	users    usersSnap
	albums   albumsSnap
	content  contentSnap
	jobs     jobsSnap
}

func newSnapshot() *snapshot {
	return &snapshot{
		health: healthSnap{collectors: map[string]collectorStat{}},
		server: serverSnap{features: map[string]float64{}},
		assets: assetsSnap{
			byType:        map[string]float64{},
			storageByType: map[string]float64{},
			byYear:        map[string]float64{},
			byRating:      map[string]float64{},
		},
		geo: geoSnap{
			byCountry:        map[string]float64{},
			countryCentroids: map[string]latlon{},
			byCity:           map[cityKey]float64{},
			cityCentroids:    map[cityKey]latlon{},
		},
		ops:      opsSnap{integrity: map[string]float64{}},
		metadata: metadataSnap{bySize: map[string]float64{}},
		users:    usersSnap{byStatus: map[string]float64{}, byRole: map[string]float64{}},
		albums: albumsSnap{
			sharedLinks: sharedLinksSnap{byType: map[string]float64{}},
			partners:    map[string]float64{},
		},
		content: contentSnap{notifByLevel: map[string]float64{}, workflows: map[string]float64{}},
		jobs:    jobsSnap{paused: map[string]float64{}},
	}
}

// allDescs is filled by desc() as each descriptor variable is initialized, so
// Describe can never fall out of sync with the declarations below.
var allDescs []*prometheus.Desc

func desc(name, help string, labels ...string) *prometheus.Desc {
	d := prometheus.NewDesc(prometheus.BuildFQName(ns, "", name), help, labels, nil)
	allDescs = append(allDescs, d)
	return d
}

// Metric descriptors. Gauges use plural-noun names (no _total, since the values
// can go down); counters carry _total; byte values carry _bytes.
var (
	// exporter self-health
	dExporterUp        = desc("exporter_up", "1 if the last Immich poll fully succeeded.")
	dScrapeDuration    = desc("exporter_last_scrape_duration_seconds", "Duration of the last Immich poll, in seconds.")
	dLastSuccess       = desc("exporter_last_success_timestamp_seconds", "Unix time of the last fully-successful poll.")
	dScrapeErrors      = desc("exporter_scrape_errors_total", "Cumulative count of failed Immich API calls.")
	dExporterBuildInfo = desc("exporter_build_info", "Exporter build metadata (value is always 1).", "version", "goversion")
	dKeyIsAdmin        = desc("key_is_admin", "1 if the configured API key belongs to an admin user (unlocks instance-wide and job metrics).")
	dCollectorSuccess  = desc("exporter_collector_success", "1 if the collector's latest run got its data; 0 if it failed, including when the API key may not use it or the server lacks it.", "collector")
	dCollectorDuration = desc("exporter_collector_duration_seconds", "Duration of the collector's latest run, in seconds.", "collector")
)

// collector is a prometheus.Collector that emits the latest snapshot fresh on
// every scrape via const metrics — no stale-series bookkeeping required.
type collector struct {
	snap         *atomic.Pointer[snapshot]
	scrapeErrors *atomic.Int64
	version      string
	goVersion    string
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range allDescs {
		ch <- d
	}
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	emit(ch, dExporterBuildInfo, 1, c.version, c.goVersion)
	ch <- prometheus.MustNewConstMetric(dScrapeErrors, prometheus.CounterValue, float64(c.scrapeErrors.Load()))

	s := c.snap.Load()
	if s == nil {
		// Before the first poll ends nothing is known, and a 0 would read as Immich being down.
		return
	}
	emit(ch, dExporterUp, s.health.scrapeSuccess)
	emit(ch, dScrapeDuration, s.health.scrapeDurationSeconds)
	if s.health.lastSuccessUnix > 0 {
		emit(ch, dLastSuccess, s.health.lastSuccessUnix)
	}
	emitOpt(ch, dKeyIsAdmin, s.health.keyIsAdmin)
	for name, st := range s.health.collectors {
		emit(ch, dCollectorSuccess, boolf(st.ok), name)
		emit(ch, dCollectorDuration, st.seconds, name)
	}

	emitServer(ch, s)
	emitOps(ch, s)
	emitAssets(ch, s)
	emitCameras(ch, s)
	emitGeo(ch, s)
	emitPeople(ch, s)
	emitUsers(ch, s)
	emitAlbums(ch, s)
	emitContent(ch, s)
	emitJobs(ch, s)
}

// emit drops (and logs) a sample whose label count does not match its Desc
// instead of panicking inside the /metrics handler.
func emit(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, lv ...string) {
	m, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, v, lv...)
	if err != nil {
		slog.Error("metric dropped", "desc", d.String(), "labels", lv, "err", err)
		return
	}
	ch <- m
}

func emitOpt(ch chan<- prometheus.Metric, d *prometheus.Desc, o opt) {
	if o.ok {
		emit(ch, d, o.v)
	}
}

// Exporter bundles the collector with its background poller, sharing one
// snapshot pointer and scrape-error counter. It satisfies prometheus.Collector
// through the embedded collector.
type Exporter struct {
	*collector
	poller *poller
}

// New wires a collector and poller around a shared snapshot and returns an
// Exporter ready to register and Run.
func New(c *immich.Client, version string, cfg Config) *Exporter {
	var snap atomic.Pointer[snapshot]
	var scrapeErrors atomic.Int64
	col := &collector{snap: &snap, scrapeErrors: &scrapeErrors, version: version, goVersion: runtime.Version()}
	p := &poller{c: c, cfg: cfg, snap: &snap, scrapeErrors: &scrapeErrors}
	return &Exporter{collector: col, poller: p}
}

// Run polls the Immich API until ctx is canceled.
func (e *Exporter) Run(ctx context.Context) { e.poller.run(ctx) }
