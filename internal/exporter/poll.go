package exporter

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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
	retried       bool // the breakdowns that failed in the last refresh have had their one retry

	// unavailable holds the collectors last refused for permission or missing
	// on the server, so that state is logged once rather than every poll.
	unavailable map[string]bool

	// filter is whether the server polled reads StatisticsSearchDto.filter; an older one would count every asset.
	filter bool
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
	refused := err != nil && unavailable(err, hard)
	r.s.health.collectors[name] = collectorStat{ok: err == nil, refused: refused, seconds: time.Since(start).Seconds()}
	if r.p.unavailable == nil {
		r.p.unavailable = map[string]bool{}
	}
	switch {
	case err == nil:
		if r.p.unavailable[name] {
			delete(r.p.unavailable, name)
			slog.Info("collector available again", "collector", name)
		}
	case refused:
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

// reachable pings Immich and reads its maintenance mode, and reports whether the rest of the poll would get answers.
func (r *pollRun) reachable() bool {
	err := r.p.ping(r.ctx)
	r.s.health.serverUp = boolf(err == nil)
	// An error status is still an answer: Immich in maintenance gives one to everything but the maintenance status.
	if _, answered := errors.AsType[*immich.APIError](err); err == nil || answered {
		r.soft("maintenance", r.p.collectMaintenance)
		if r.s.server.maintenance.v == 1 {
			return false
		}
	}
	if err != nil {
		r.hardErrs++
		r.p.scrapeErrors.Add(1)
		slog.Error("Immich does not answer; skipping this poll", "err", err)
		return false
	}
	return true
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

	if !r.reachable() {
		// In maintenance or down, Immich would fail every other call the same way; one log line says it all.
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
	p.filter = searchFilter(s.server.about.version)
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
	var peopleErr error
	r.hard("people", func(ctx context.Context, s *snapshot) error {
		people, peopleErr = p.collectPeople(ctx, s)
		return peopleErr
	})

	admin := s.health.keyIsAdmin.v == 1
	p.refreshBreakdowns(ctx, s, p.breakdowns(people, peopleErr, admin))

	if admin {
		r.soft("admin/users", p.collectUsers)
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

// breakdown is a slow-tier collector and the snapshot section it fills, which fast snapshots carry forward.
type breakdown struct {
	name    string
	collect collectFunc
	carry   func(to, from *snapshot)
}

func (p *poller) breakdowns(people []person, peopleErr error, admin bool) []breakdown {
	var bs []breakdown
	add := func(enabled bool, name string, collect collectFunc, carry func(to, from *snapshot)) {
		if enabled {
			bs = append(bs, breakdown{name: name, collect: collect, carry: carry})
		}
	}
	add(p.cfg.CollectCamera, "cameras", p.collectCameras, func(to, from *snapshot) { to.cameras = from.cameras })
	add(p.cfg.CollectGeo, "geo", p.collectGeo, func(to, from *snapshot) { to.geo = from.geo })
	add(p.cfg.CollectPeople, "people/statistics", func(ctx context.Context, s *snapshot) error {
		if peopleErr != nil {
			return fmt.Errorf("no people to count assets for: %w", peopleErr)
		}
		return p.collectPersonStats(ctx, people, s)
	}, func(to, from *snapshot) { to.people.assets = from.people.assets })
	add(p.cfg.CollectHeavy, "duplicates", p.collectDuplicates, func(to, from *snapshot) {
		t, f := &to.content, &from.content
		t.duplicateSets, t.duplicateAssets, t.duplicatesOK = f.duplicateSets, f.duplicateAssets, f.duplicatesOK
	})
	add(p.cfg.CollectHeavy, "stacks", p.collectStacks, func(to, from *snapshot) {
		t, f := &to.content, &from.content
		t.stacks, t.stackedAssets, t.stacksOK = f.stacks, f.stackedAssets, f.stacksOK
	})
	add(p.filter, "assets/metadata", p.collectMetadata, func(to, from *snapshot) { to.metadata = from.metadata })
	add(admin, "admin/users/statistics", p.collectUserAssets, func(to, from *snapshot) {
		to.users.assets, to.assets.locked = from.users.assets, from.assets.locked
	})
	return bs
}

// refreshBreakdowns retries a failed breakdown once before the next refresh: a blip must not cost the whole interval.
func (p *poller) refreshBreakdowns(ctx context.Context, s *snapshot, bs []breakdown) {
	due := p.bd == nil || time.Since(p.lastBreakdown) >= p.cfg.BreakdownInterval
	if due {
		p.bd, p.retried = newSnapshot(), false
	}
	var retrying bool
	for _, b := range bs {
		last, ran := p.bd.health.collectors[b.name]
		failed := ran && !last.ok && !last.refused
		if due || (failed && !p.retried) {
			retrying = retrying || !due
			fresh := newSnapshot()
			(&pollRun{p: p, ctx: ctx, s: fresh}).soft(b.name, b.collect)
			b.carry(p.bd, fresh)
			p.bd.health.collectors[b.name] = fresh.health.collectors[b.name]
		}
		b.carry(s, p.bd)
	}
	if due {
		p.lastBreakdown = time.Now()
	}
	p.retried = p.retried || retrying
	maps.Copy(s.health.collectors, p.bd.health.collectors)
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
