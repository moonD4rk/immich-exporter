package exporter

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moond4rk/immich-exporter/internal/immich"
)

// Config controls what the poller collects and how aggressively.
type Config struct {
	Interval          time.Duration
	BreakdownInterval time.Duration
	CollectCamera     bool
	CollectGeo        bool
	CollectRatings    bool
	CollectPeople     bool
	CollectHeavy      bool
	TopN              int
	FanoutLimit       int
	FanoutConcurrency int
	CityLimit         int
}

type poller struct {
	c            *immich.Client
	cfg          Config
	snap         *atomic.Pointer[snapshot]
	scrapeErrors *atomic.Int64

	// Expensive collectors (fan-outs, whole-library listings) refresh on the
	// slower breakdownInterval into bd, whose sections every fast snapshot
	// carries forward. Owned by the single run() goroutine, like unavailable.
	bd            *snapshot
	lastBreakdown time.Time

	// unavailable holds the collectors last refused for permission or missing
	// on the server, so that state is logged once rather than every poll.
	unavailable map[string]bool
}

// collectFunc fills its part of a snapshot from the Immich API.
type collectFunc func(context.Context, *snapshot) error

// pollRun runs the collectors of one poll into s, recording each one's outcome.
type pollRun struct {
	p        *poller
	ctx      context.Context
	s        *snapshot
	hardErrs int
}

// hard runs a collector whose failure means the poll did not fully succeed. A
// 403 or 404 is still only "unavailable": a key scoped without that permission
// would otherwise keep immich_exporter_up at 0 forever.
func (r *pollRun) hard(name string, fn collectFunc) { r.collect(name, true, fn) }

func (r *pollRun) soft(name string, fn collectFunc) { r.collect(name, false, fn) }

func (r *pollRun) collect(name string, hard bool, fn collectFunc) {
	start := time.Now()
	err := fn(r.ctx, r.s)
	r.s.health.collectors[name] = collectorStat{ok: err == nil, seconds: time.Since(start).Seconds()}
	if r.p.unavailable == nil {
		r.p.unavailable = map[string]bool{}
	}
	switch {
	case err == nil:
		if r.p.unavailable[name] {
			delete(r.p.unavailable, name)
			slog.Info("collector available again", "collector", name)
		}
	case unavailable(err, hard):
		if !r.p.unavailable[name] {
			r.p.unavailable[name] = true
			slog.Warn("collector unavailable: the API key lacks the permission or the server lacks the endpoint",
				"collector", name, "err", err)
		}
	case hard:
		r.hardErrs++
		r.p.scrapeErrors.Add(1)
		slog.Error("collect failed", "collector", name, "err", err)
	default:
		r.p.scrapeErrors.Add(1)
		slog.Warn("optional collect failed", "collector", name, "err", err)
	}
}

// unavailable reports a refusal rather than a fault. A hard collector's 401 is
// left out: there it means the API key itself is invalid.
func unavailable(err error, hard bool) bool {
	if hard {
		return immich.StatusIs(err, 403, 404)
	}
	return isSoftStatus(err)
}

// run polls the Immich API every interval until the context is canceled. The
// loop is sequential, so a slow poll delays the next one rather than overlapping.
func (p *poller) run(ctx context.Context) {
	for {
		p.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.cfg.Interval):
		}
	}
}

func (p *poller) poll(parent context.Context) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()

	s := newSnapshot()
	r := &pollRun{p: p, ctx: ctx, s: s}

	s.health.serverUp = boolf(p.serverUp(ctx))
	r.soft("maintenance", p.collectMaintenance)
	if s.server.maintenance.v == 1 {
		// Immich answers little but this status during maintenance; polling the rest would only log failures.
		p.finish(s, start, r.hardErrs)
		return
	}

	var meErr error
	r.hard("users/me", func(ctx context.Context, s *snapshot) error {
		meErr = p.collectMe(ctx, s)
		return meErr
	})
	if immich.StatusIs(meErr, 401) {
		// Every other call carries the same rejected key; stopping keeps the log to this one error.
		p.finish(s, start, r.hardErrs)
		return
	}
	r.hard("server/about", p.collectAbout)
	r.soft("server/version-check", p.collectVersionCheck)
	r.soft("server/version-history", p.collectVersionHistory)
	r.soft("server/features", p.collectFeatures)
	r.soft("server/config", p.collectConfig)
	r.soft("server/storage", p.collectStorage)

	r.hard("assets", p.collectAssets)
	r.soft("assets/state", p.collectAssetStates)
	if p.cfg.CollectRatings {
		r.soft("assets/rating", p.collectRatings)
	}
	r.soft("timeline/buckets", p.collectYears)

	var people []person
	r.hard("people", func(ctx context.Context, s *snapshot) (err error) {
		people, err = p.collectPeople(ctx, s)
		return err
	})

	if p.bd == nil || time.Since(p.lastBreakdown) >= p.cfg.BreakdownInterval {
		p.bd = p.collectBreakdowns(ctx, people, s.server.about.version)
		p.lastBreakdown = time.Now()
	}
	applyBreakdown(s, p.bd)

	if s.health.keyIsAdmin == 1 {
		r.soft("admin/users", p.collectUsers)
		r.soft("assets/locked", p.collectLocked)
		r.hard("jobs", p.collectJobs)
		r.soft("integrity", p.collectIntegrity)
		r.soft("database-backups", p.collectBackups)
	}

	r.hard("albums", p.collectAlbums)
	r.soft("albums/statistics", p.collectAlbumStats)
	r.soft("shared-links", p.collectSharedLinks)
	r.soft("partners", p.collectPartners)

	r.soft("tags", p.collectTags)
	r.soft("memories", p.collectMemories)
	r.soft("libraries", p.collectLibraries)
	r.soft("api-keys", p.collectAPIKeys)
	r.soft("sessions", p.collectSessions)
	r.soft("notifications", p.collectNotifications)
	r.soft("workflows", p.collectWorkflows)
	r.soft("plugins", p.collectPlugins)

	p.finish(s, start, r.hardErrs)
}

func (p *poller) finish(s *snapshot, start time.Time, hardErrs int) {
	s.health.scrapeDurationSeconds = time.Since(start).Seconds()
	if hardErrs == 0 {
		s.health.scrapeSuccess = 1
		s.health.lastSuccessUnix = float64(time.Now().Unix())
	} else if prev := p.snap.Load(); prev != nil {
		s.health.lastSuccessUnix = prev.health.lastSuccessUnix // preserve last good timestamp
	}
	p.snap.Store(s)
}

// collectBreakdowns runs the expensive collectors (per-value fan-outs, and
// listings that return the whole library) into a fresh snapshot, whose
// sections applyBreakdown carries into every fast snapshot until the next
// refresh. A failed collector leaves its section empty (consistent with the
// "no stale values on failure" policy). people is the list collectPeople
// fetched this poll (nil when that failed); version gates SearchFilter.
func (p *poller) collectBreakdowns(ctx context.Context, people []person, version string) *snapshot {
	bd := newSnapshot()
	r := &pollRun{p: p, ctx: ctx, s: bd}
	if p.cfg.CollectCamera {
		r.soft("cameras", p.collectCameras)
	}
	if p.cfg.CollectGeo {
		r.soft("geo", p.collectGeo)
	}
	if p.cfg.CollectPeople {
		r.soft("people/statistics", func(ctx context.Context, s *snapshot) error {
			return p.collectPersonStats(ctx, people, s)
		})
	}
	if p.cfg.CollectHeavy {
		r.soft("duplicates", p.collectDuplicates)
		r.soft("stacks", p.collectStacks)
	}
	if searchFilter(version) {
		r.soft("assets/metadata", p.collectMetadata)
	}
	return bd
}

func applyBreakdown(s, bd *snapshot) {
	if bd == nil {
		return
	}
	s.cameras = bd.cameras
	s.geo = bd.geo
	s.metadata = bd.metadata
	s.people.assets = bd.people.assets
	c, b := &s.content, &bd.content
	c.duplicateSets, c.duplicateAssets, c.duplicatesOK = b.duplicateSets, b.duplicateAssets, b.duplicatesOK
	c.stacks, c.stackedAssets, c.stacksOK = b.stacks, b.stackedAssets, b.stacksOK
	maps.Copy(s.health.collectors, bd.health.collectors)
}

// --- helpers ---

// parallel applies fn to every item with at most limit goroutines in flight
// and returns the results fn accepted, in completion order.
func parallel[T, R any](items []T, limit int, fn func(T) (R, bool)) []R {
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, limit)
	out := make([]R, 0, len(items))
	for _, it := range items {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if r, ok := fn(it); ok {
				mu.Lock()
				out = append(out, r)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return out
}

// isSoftStatus reports HTTP failures treated as "optional/expected": 401/403
// (permission- or elevated-auth-gated) and 404 (endpoint absent on this version).
func isSoftStatus(err error) bool { return immich.StatusIs(err, 401, 403, 404) }

func boolf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func topNWithOther(m map[string]float64, n int) map[string]float64 {
	if n <= 0 || len(m) <= n {
		return m
	}
	type kv struct {
		k string
		v float64
	}
	arr := make([]kv, 0, len(m))
	for k, v := range m {
		arr = append(arr, kv{k, v})
	}
	slices.SortFunc(arr, func(a, b kv) int {
		return cmp.Or(cmp.Compare(b.v, a.v), cmp.Compare(a.k, b.k))
	})
	out := make(map[string]float64, n+1)
	var other float64
	for i, e := range arr {
		if i < n {
			out[e.k] = e.v
		} else {
			other += e.v
		}
	}
	if other > 0 {
		out["other"] = other
	}
	return out
}

func topNLabeled(vals []labeledVal, n int) []labeledVal {
	slices.SortFunc(vals, func(a, b labeledVal) int {
		return cmp.Or(cmp.Compare(b.value, a.value), cmp.Compare(a.id, b.id))
	})
	if n > 0 && len(vals) > n {
		vals = vals[:n]
	}
	return vals
}
