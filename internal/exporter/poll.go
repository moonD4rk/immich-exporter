package exporter

import (
	"cmp"
	"context"
	"iter"
	"log/slog"
	"slices"
	"strconv"
	"strings"
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
}

type poller struct {
	c            *immich.Client
	cfg          Config
	snap         *atomic.Pointer[snapshot]
	scrapeErrors *atomic.Int64

	// Expensive fan-out breakdowns (cameras, per-person stats) refresh on the
	// slower breakdownInterval; the latest result is carried forward into every
	// fast snapshot. Owned by the single run() goroutine, so no lock is needed.
	bd            *breakdownData
	lastBreakdown time.Time
}

// breakdownData caches the slow-tier fan-out results between refreshes.
type breakdownData struct {
	cameras      camerasSnap
	personAssets []labeledVal
}

type person struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	BirthDate *string `json:"birthDate"`
}

type peoplePage struct {
	People      []person `json:"people"`
	HasNextPage bool     `json:"hasNextPage"`
	Total       float64  `json:"total"`
	Hidden      float64  `json:"hidden"`
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
	var hardErrs int
	hard := func(name string, err error) {
		if err == nil {
			return
		}
		hardErrs++
		p.scrapeErrors.Add(1)
		slog.Error("collect failed", "section", name, "err", err)
	}
	soft := func(name string, err error) {
		if err == nil || isSoftStatus(err) {
			return
		}
		p.scrapeErrors.Add(1)
		slog.Warn("optional collect failed", "section", name, "err", err)
	}

	s.health.serverUp = boolf(p.serverUp(ctx))

	isAdmin, err := p.collectMe(ctx, s)
	hard("users/me", err)

	hard("server/about", p.collectAbout(ctx, s))
	soft("server/version-check", p.collectVersionCheck(ctx, s))
	soft("server/features", p.collectFeatures(ctx, s))
	soft("server/config", p.collectConfig(ctx, s))
	soft("server/storage", p.collectStorage(ctx, s))

	hard("assets", p.collectAssets(ctx, s, isAdmin))
	soft("assets/state", p.collectAssetStates(ctx, s))
	if p.cfg.CollectRatings {
		soft("assets/rating", p.collectRatings(ctx, s))
	}
	soft("timeline/buckets", p.collectYears(ctx, s))

	if p.cfg.CollectGeo {
		soft("geo", p.collectGeo(ctx, s))
	}

	people, err := p.collectPeople(ctx, s)
	hard("people", err)

	// Slow tier: refresh expensive fan-outs only every breakdownInterval and
	// carry the last result forward into this snapshot.
	if p.bd == nil || time.Since(p.lastBreakdown) >= p.cfg.BreakdownInterval {
		p.bd = p.collectBreakdowns(ctx, soft, people)
		p.lastBreakdown = time.Now()
	}
	applyBreakdown(s, p.bd)

	if isAdmin {
		soft("admin/users", p.collectUsers(ctx, s))
		hard("jobs", p.collectJobs(ctx, s))
	}

	hard("albums", p.collectAlbums(ctx, s))
	soft("albums/statistics", p.collectAlbumStats(ctx, s))
	soft("shared-links", p.collectSharedLinks(ctx, s))
	soft("partners", p.collectPartners(ctx, s))

	soft("tags", p.collectTags(ctx, s))
	soft("memories", p.collectMemories(ctx, s))
	soft("libraries", p.collectLibraries(ctx, s))
	soft("api-keys", p.collectAPIKeys(ctx, s))
	soft("sessions", p.collectSessions(ctx, s))
	soft("notifications", p.collectNotifications(ctx, s))
	if p.cfg.CollectHeavy {
		soft("duplicates", p.collectDuplicates(ctx, s))
		soft("stacks", p.collectStacks(ctx, s))
	}

	s.health.scrapeDurationSeconds = time.Since(start).Seconds()
	if hardErrs == 0 {
		s.health.scrapeSuccess = 1
		s.health.lastSuccessUnix = float64(time.Now().Unix())
	} else if prev := p.snap.Load(); prev != nil {
		s.health.lastSuccessUnix = prev.health.lastSuccessUnix // preserve last good timestamp
	}
	p.snap.Store(s)
}

func (p *poller) serverUp(ctx context.Context) bool {
	var out struct {
		Res string `json:"res"`
	}
	return p.c.Get(ctx, "/server/ping", &out) == nil && out.Res == "pong"
}

func (p *poller) collectMe(ctx context.Context, s *snapshot) (bool, error) {
	var me struct {
		IsAdmin bool `json:"isAdmin"`
	}
	if err := p.c.Get(ctx, "/users/me", &me); err != nil {
		return false, err
	}
	s.health.keyIsAdmin = boolf(me.IsAdmin)
	return me.IsAdmin, nil
}

func (p *poller) collectAbout(ctx context.Context, s *snapshot) error {
	var a struct {
		Version      string `json:"version"`
		SourceRef    string `json:"sourceRef"`
		SourceCommit string `json:"sourceCommit"`
		Build        string `json:"build"`
		Nodejs       string `json:"nodejs"`
		Exiftool     string `json:"exiftool"`
		Ffmpeg       string `json:"ffmpeg"`
		Imagemagick  string `json:"imagemagick"`
		Libvips      string `json:"libvips"`
		Licensed     bool   `json:"licensed"`
	}
	if err := p.c.Get(ctx, "/server/about", &a); err != nil {
		return err
	}
	s.server.about = aboutInfo{
		version: a.Version, sourceRef: a.SourceRef, sourceCommit: a.SourceCommit, build: a.Build,
		nodejs: a.Nodejs, exiftool: a.Exiftool, ffmpeg: a.Ffmpeg, imagemagick: a.Imagemagick,
		libvips: a.Libvips, ok: true,
	}
	s.server.licensed = boolf(a.Licensed)
	return nil
}

func (p *poller) collectVersionCheck(ctx context.Context, s *snapshot) error {
	var vc struct {
		ReleaseVersion string `json:"releaseVersion"`
	}
	if err := p.c.Get(ctx, "/server/version-check", &vc); err != nil {
		return err
	}
	if vc.ReleaseVersion == "" {
		return nil
	}
	s.server.latestVersion = vc.ReleaseVersion
	// version-check can lag and report a release OLDER than the running server;
	// only a strictly newer release counts as an available update. A stable
	// release also supersedes a running pre-release of the same core version.
	if release, relPre, ok := parseSemver(vc.ReleaseVersion); ok {
		if running, runPre, ok := parseSemver(s.server.about.version); ok {
			if semverGreater(release, running) || (release == running && runPre && !relPre) {
				s.server.updateAvail = 1
			}
		}
	}
	return nil
}

func (p *poller) collectFeatures(ctx context.Context, s *snapshot) error {
	var f map[string]bool
	if err := p.c.Get(ctx, "/server/features", &f); err != nil {
		return err
	}
	for k, v := range f {
		s.server.features[camelToSnake(k)] = boolf(v)
	}
	return nil
}

func (p *poller) collectConfig(ctx context.Context, s *snapshot) error {
	var c struct {
		TrashDays       *float64 `json:"trashDays"`
		UserDeleteDelay *float64 `json:"userDeleteDelay"`
		MinFaces        *float64 `json:"minFaces"`
		IsInitialized   *bool    `json:"isInitialized"`
		IsOnboarded     *bool    `json:"isOnboarded"`
	}
	if err := p.c.Get(ctx, "/server/config", &c); err != nil {
		return err
	}
	if c.TrashDays != nil {
		s.server.trashDays = set(*c.TrashDays)
	}
	if c.UserDeleteDelay != nil {
		s.server.userDeleteDelayDays = set(*c.UserDeleteDelay)
	}
	if c.MinFaces != nil {
		s.server.minFaces = set(*c.MinFaces)
	}
	if c.IsInitialized != nil {
		s.server.initialized = set(boolf(*c.IsInitialized))
	}
	if c.IsOnboarded != nil {
		s.server.onboarded = set(boolf(*c.IsOnboarded))
	}
	return nil
}

func (p *poller) collectStorage(ctx context.Context, s *snapshot) error {
	var st struct {
		DiskSizeRaw      float64 `json:"diskSizeRaw"`
		DiskUseRaw       float64 `json:"diskUseRaw"`
		DiskAvailableRaw float64 `json:"diskAvailableRaw"`
	}
	if err := p.c.Get(ctx, "/server/storage", &st); err != nil {
		return err
	}
	s.server.storageSizeBytes = set(st.DiskSizeRaw)
	s.server.storageUsedBytes = set(st.DiskUseRaw)
	s.server.storageAvailableBytes = set(st.DiskAvailableRaw)
	return nil
}

type serverStats struct {
	Photos      float64 `json:"photos"`
	Videos      float64 `json:"videos"`
	Usage       float64 `json:"usage"`
	UsagePhotos float64 `json:"usagePhotos"`
	UsageVideos float64 `json:"usageVideos"`
	UsageByUser []struct {
		UserID           string   `json:"userId"`
		UserName         string   `json:"userName"`
		Photos           float64  `json:"photos"`
		Videos           float64  `json:"videos"`
		Usage            float64  `json:"usage"`
		QuotaSizeInBytes *float64 `json:"quotaSizeInBytes"`
	} `json:"usageByUser"`
}

func (p *poller) collectAssets(ctx context.Context, s *snapshot, isAdmin bool) error {
	if isAdmin {
		handled, err := p.collectServerStats(ctx, s)
		if err != nil {
			return err
		}
		if handled {
			return nil
		}
	}
	// Owner-scoped fallback (non-admin key, or admin key lacking server.statistics).
	for _, t := range []string{"IMAGE", "VIDEO"} {
		n, err := p.c.StatTotal(ctx, map[string]any{"type": t})
		if err != nil {
			return err
		}
		s.assets.byType[t] = n
	}
	return nil
}

// collectServerStats fills instance-wide and per-user volumetrics from the
// admin-only /server/statistics. It returns handled=false with no error when
// the key lacks access (401/403), so the caller can fall back to owner scope.
func (p *poller) collectServerStats(ctx context.Context, s *snapshot) (bool, error) {
	var st serverStats
	if err := p.c.Get(ctx, "/server/statistics", &st); err != nil {
		if immich.StatusIs(err, 401, 403) {
			return false, nil
		}
		return false, err
	}
	s.assets.byType["IMAGE"], s.assets.byType["VIDEO"] = st.Photos, st.Videos
	s.assets.storageByType["IMAGE"], s.assets.storageByType["VIDEO"] = st.UsagePhotos, st.UsageVideos
	s.assets.serverUsageBytes = set(st.Usage)
	for _, u := range st.UsageByUser {
		us := userStat{id: u.UserID, name: u.UserName, photos: u.Photos, videos: u.Videos, usageBytes: u.Usage}
		if u.QuotaSizeInBytes == nil {
			us.quotaUnlimited = true
		} else {
			us.quotaBytes = *u.QuotaSizeInBytes
		}
		s.users.perUser = append(s.users.perUser, us)
	}
	return true, nil
}

func (p *poller) collectAssetStates(ctx context.Context, s *snapshot) error {
	a := &s.assets
	type q struct {
		v      *opt
		filter map[string]any
	}
	checks := []q{
		{&a.favorite, map[string]any{"isFavorite": true}},
		{&a.archived, map[string]any{"visibility": "archive"}},
		{&a.hidden, map[string]any{"visibility": "hidden"}},
		{&a.locked, map[string]any{"visibility": "locked"}},
		{&a.offline, map[string]any{"isOffline": true}},
		{&a.motion, map[string]any{"isMotion": true}},
		{&a.notInAlbum, map[string]any{"isNotInAlbum": true}},
		{&a.encoded, map[string]any{"isEncoded": true}},
	}
	// visibility=locked 401s under API-key auth (PIN-protected folder); skip
	// filters the server rejects so one denial can't drop the other states.
	for _, c := range checks {
		n, err := p.c.StatTotal(ctx, c.filter)
		if err != nil {
			if isSoftStatus(err) {
				continue
			}
			return err
		}
		*c.v = set(n)
	}
	// Trashed has no isTrashed boolean in StatisticsSearchDto; use /assets/statistics.
	var ts struct {
		Total float64 `json:"total"`
	}
	if err := p.c.Get(ctx, "/assets/statistics?isTrashed=true", &ts); err == nil {
		a.trashed = set(ts.Total)
	}
	return nil
}

func (p *poller) collectRatings(ctx context.Context, s *snapshot) error {
	for r := 1; r <= 5; r++ {
		n, err := p.c.StatTotal(ctx, map[string]any{"rating": r})
		if err != nil {
			return err
		}
		s.assets.byRating[strconv.Itoa(r)] = n
	}
	n, err := p.c.StatTotal(ctx, map[string]any{"rating": nil})
	if err != nil {
		return err
	}
	s.assets.byRating["unrated"] = n
	return nil
}

func (p *poller) collectYears(ctx context.Context, s *snapshot) error {
	var buckets []struct {
		TimeBucket string  `json:"timeBucket"`
		Count      float64 `json:"count"`
	}
	// v3 ignores size; pre-v1.133 servers require it — keep it for both.
	if err := p.c.Get(ctx, "/timeline/buckets?size=MONTH", &buckets); err != nil {
		return err
	}
	for _, b := range buckets {
		if len(b.TimeBucket) >= 4 {
			s.assets.byYear[b.TimeBucket[:4]] += b.Count
		}
	}
	return nil
}

// collectBreakdowns runs the expensive fan-out collectors into a fresh
// breakdownData. A failed sub-collector leaves its section empty (consistent
// with the "no stale values on failure" policy); between refreshes the last
// good result is carried forward by the caller. people is the list already
// fetched by collectPeople this poll (nil when that fetch failed).
func (p *poller) collectBreakdowns(ctx context.Context, soft func(string, error), people []person) *breakdownData {
	bd := &breakdownData{}
	if p.cfg.CollectCamera {
		soft("cameras", p.collectCameras(ctx, bd))
	}
	if p.cfg.CollectPeople {
		p.collectPersonStats(ctx, people, bd)
	}
	return bd
}

func applyBreakdown(s *snapshot, bd *breakdownData) {
	if bd == nil {
		return
	}
	s.cameras = bd.cameras
	s.people.assets = bd.personAssets
}

func (p *poller) collectCameras(ctx context.Context, bd *breakdownData) error {
	makes, err := p.c.Suggest(ctx, "camera-make")
	if err != nil {
		return err
	}
	bd.cameras.makes = set(float64(len(makes)))
	models, err := p.c.Suggest(ctx, "camera-model")
	if err != nil {
		return err
	}
	bd.cameras.models = set(float64(len(models)))
	lenses, err := p.c.Suggest(ctx, "camera-lens-model")
	if err != nil {
		return err
	}
	bd.cameras.lenses = set(float64(len(lenses)))

	bd.cameras.byMake = p.fanout(ctx, makes, "make", 0) // makes are low-cardinality: emit all
	bd.cameras.byModel = p.fanout(ctx, models, "model", p.cfg.TopN)
	bd.cameras.byLens = p.fanout(ctx, lenses, "lensModel", p.cfg.TopN)
	return nil
}

// fanout counts assets per distinct value via search/statistics, with bounded
// concurrency. If the value list exceeds fanoutLimit it is skipped (returns nil)
// to bound API load. When n>0 only the top-n by count are kept, the rest folded
// into an "other" bucket.
func (p *poller) fanout(ctx context.Context, values []string, field string, n int) map[string]float64 {
	if len(values) == 0 {
		return nil
	}
	if len(values) > p.cfg.FanoutLimit {
		slog.Warn("fanout skipped: too many distinct values", "field", field, "values", len(values), "limit", p.cfg.FanoutLimit)
		return nil
	}
	type kv struct {
		k string
		v float64
	}
	counts := parallel(values, p.cfg.FanoutConcurrency, func(v string) (kv, bool) {
		c, err := p.c.StatTotal(ctx, map[string]any{field: v})
		if err != nil {
			slog.Warn("fanout call failed", "field", field, "value", v, "err", err)
			return kv{}, false
		}
		return kv{v, c}, true
	})
	m := make(map[string]float64, len(counts))
	for _, e := range counts {
		m[e.k] = e.v
	}
	return topNWithOther(m, n)
}

func (p *poller) collectGeo(ctx context.Context, s *snapshot) error {
	g := &s.geo
	if cities, err := p.c.Suggest(ctx, "city"); err == nil {
		g.cities = set(float64(len(cities)))
	}
	if states, err := p.c.Suggest(ctx, "state"); err == nil {
		g.states = set(float64(len(states)))
	}
	if countries, err := p.c.Suggest(ctx, "country"); err == nil {
		g.countries = set(float64(len(countries)))
	}
	// One /map/markers call yields the geotagged total, the per-country and
	// per-city splits, and each group's asset centroid (mean lat/lon) for a
	// coords-mode geomap — no place-name → ISO mapping required.
	var markers []struct {
		City    string  `json:"city"`
		Country string  `json:"country"`
		Lat     float64 `json:"lat"`
		Lon     float64 `json:"lon"`
	}
	if err := p.c.Get(ctx, "/map/markers", &markers); err != nil {
		return err
	}
	g.geotagged = set(float64(len(markers)))
	type acc struct{ sumLat, sumLon, count float64 }
	countryAgg := map[string]*acc{}
	cityAgg := map[cityKey]*acc{}
	for _, m := range markers {
		country := strings.TrimSpace(m.Country)
		if country == "" {
			country = "unknown"
		}
		city := strings.TrimSpace(m.City)
		if city == "" {
			city = "unknown"
		}
		ck := cityKey{city: city, country: country}
		g.byCountry[country]++
		g.byCity[ck]++
		if country != "unknown" {
			a := countryAgg[country]
			if a == nil {
				a = &acc{}
				countryAgg[country] = a
			}
			a.sumLat, a.sumLon, a.count = a.sumLat+m.Lat, a.sumLon+m.Lon, a.count+1
		}
		if city != "unknown" {
			a := cityAgg[ck]
			if a == nil {
				a = &acc{}
				cityAgg[ck] = a
			}
			a.sumLat, a.sumLon, a.count = a.sumLat+m.Lat, a.sumLon+m.Lon, a.count+1
		}
	}
	for country, a := range countryAgg {
		// Round to ~11 km so the centroid label stays stable as assets are added.
		g.countryCentroids[country] = latlon{
			lat: strconv.FormatFloat(a.sumLat/a.count, 'f', 1, 64),
			lon: strconv.FormatFloat(a.sumLon/a.count, 'f', 1, 64),
		}
	}
	for ck, a := range cityAgg {
		// ~1.1 km: city clusters are tight, so finer rounding stays stable while
		// keeping the marker inside the city.
		g.cityCentroids[ck] = latlon{
			lat: strconv.FormatFloat(a.sumLat/a.count, 'f', 2, 64),
			lon: strconv.FormatFloat(a.sumLon/a.count, 'f', 2, 64),
		}
	}
	return nil
}

// peoplePages walks GET /people one page at a time. The first error is yielded
// and ends the walk, as does the last page or the caller breaking out.
func (p *poller) peoplePages(ctx context.Context) iter.Seq2[peoplePage, error] {
	return func(yield func(peoplePage, error) bool) {
		for page := 1; ; page++ {
			var pg peoplePage
			err := p.c.Get(ctx, "/people?withHidden=true&size=1000&page="+strconv.Itoa(page), &pg)
			if !yield(pg, err) || err != nil || !pg.HasNextPage || len(pg.People) == 0 {
				return
			}
		}
	}
}

// collectPeople fills the people counts and returns the full list so the
// slow-tier person breakdown can reuse it instead of paging /people again.
func (p *poller) collectPeople(ctx context.Context, s *snapshot) ([]person, error) {
	var all []person
	for pg, err := range p.peoplePages(ctx) {
		if err != nil {
			return nil, err
		}
		if !s.people.ok {
			s.people.total, s.people.hidden = pg.Total, pg.Hidden
			s.people.ok = true
		}
		all = append(all, pg.People...)
	}
	var named, withBirthdate float64
	for _, pe := range all {
		if strings.TrimSpace(pe.Name) != "" {
			named++
		}
		if pe.BirthDate != nil {
			withBirthdate++
		}
	}
	s.people.named = named
	s.people.unnamed = s.people.total - named
	s.people.withBirthdate = withBirthdate
	return all, nil
}

func (p *poller) collectPersonStats(ctx context.Context, people []person, bd *breakdownData) {
	// Cap to FanoutLimit, matching the bound fanout() enforces for cameras.
	people = people[:min(len(people), p.cfg.FanoutLimit)]
	vals := parallel(people, p.cfg.FanoutConcurrency, func(pe person) (labeledVal, bool) {
		var st struct {
			Assets float64 `json:"assets"`
		}
		if err := p.c.Get(ctx, "/people/"+pe.ID+"/statistics", &st); err != nil {
			return labeledVal{}, false
		}
		name := pe.Name
		if name == "" {
			name = "(unnamed)"
		}
		return labeledVal{id: pe.ID, name: name, value: st.Assets}, true
	})
	bd.personAssets = topNLabeled(vals, p.cfg.TopN)
}

func (p *poller) collectUsers(ctx context.Context, s *snapshot) error {
	var users []struct {
		Status  string `json:"status"`
		IsAdmin bool   `json:"isAdmin"`
	}
	if err := p.c.Get(ctx, "/admin/users?withDeleted=true", &users); err != nil {
		return err
	}
	s.users.ok = true
	for _, u := range users {
		st := u.Status
		if st == "" {
			st = "active"
		}
		s.users.byStatus[st]++
		if u.IsAdmin {
			s.users.byRole["admin"]++
		} else {
			s.users.byRole["user"]++
		}
	}
	return nil
}

// queueCounts is the per-state depth shared by /queues (v2.4.0+) and the legacy
// /jobs map (v1–v3, deprecated at v2.4.0).
type queueCounts struct {
	Active    float64 `json:"active"`
	Completed float64 `json:"completed"`
	Failed    float64 `json:"failed"`
	Delayed   float64 `json:"delayed"`
	Waiting   float64 `json:"waiting"`
	Paused    float64 `json:"paused"`
}

// collectJobs prefers /queues (added v2.4.0); it falls back to the deprecated
// /jobs map on 403/404 — older servers lack /queues, and keys scoped to job.read
// (not queue.read) get 403 — so both metric shapes keep the same series.
func (p *poller) collectJobs(ctx context.Context, s *snapshot) error {
	var queues []struct {
		Name       string      `json:"name"`
		IsPaused   bool        `json:"isPaused"`
		Statistics queueCounts `json:"statistics"`
	}
	err := p.c.Get(ctx, "/queues", &queues)
	if immich.StatusIs(err, 403, 404) {
		return p.collectJobsLegacy(ctx, s)
	}
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(queues))
	for _, q := range queues {
		if seen[q.Name] {
			continue // guard against duplicate names crashing the whole /metrics scrape
		}
		seen[q.Name] = true
		addQueue(s, q.Name, q.IsPaused, q.Statistics)
	}
	return nil
}

func (p *poller) collectJobsLegacy(ctx context.Context, s *snapshot) error {
	var jobs map[string]struct {
		JobCounts   queueCounts `json:"jobCounts"`
		QueueStatus struct {
			IsPaused bool `json:"isPaused"`
		} `json:"queueStatus"`
	}
	if err := p.c.Get(ctx, "/jobs", &jobs); err != nil {
		return err
	}
	for name, j := range jobs {
		addQueue(s, name, j.QueueStatus.IsPaused, j.JobCounts)
	}
	return nil
}

func addQueue(s *snapshot, name string, paused bool, c queueCounts) {
	s.jobs.queues = append(s.jobs.queues,
		jobQueueStat{name, "active", c.Active},
		jobQueueStat{name, "completed", c.Completed},
		jobQueueStat{name, "failed", c.Failed},
		jobQueueStat{name, "delayed", c.Delayed},
		jobQueueStat{name, "waiting", c.Waiting},
		jobQueueStat{name, "paused", c.Paused},
	)
	s.jobs.paused[name] = boolf(paused)
}

func (p *poller) collectAlbums(ctx context.Context, s *snapshot) error {
	var albums []struct {
		ID            string  `json:"id"`
		AlbumName     string  `json:"albumName"`
		Shared        bool    `json:"shared"`
		AssetCount    float64 `json:"assetCount"`
		HasSharedLink bool    `json:"hasSharedLink"`
	}
	if err := p.c.Get(ctx, "/albums", &albums); err != nil {
		return err
	}
	al := &s.albums
	al.ok = true
	var sum, maxCount float64
	vals := make([]labeledVal, 0, len(albums))
	for _, a := range albums {
		if a.Shared {
			al.sharedCount++
		} else {
			al.privateCount++
		}
		sum += a.AssetCount
		if a.AssetCount > maxCount {
			maxCount = a.AssetCount
		}
		if a.AssetCount == 0 {
			al.empty++
		}
		if a.HasSharedLink {
			al.withSharedLink++
		}
		vals = append(vals, labeledVal{id: a.ID, name: a.AlbumName, value: a.AssetCount})
	}
	al.assetsTotal, al.assetsMax = sum, maxCount
	if n := len(albums); n > 0 {
		al.assetsAvg = sum / float64(n)
	}
	al.top = topNLabeled(vals, p.cfg.TopN)
	return nil
}

func (p *poller) collectAlbumStats(ctx context.Context, s *snapshot) error {
	var st struct {
		Owned     float64 `json:"owned"`
		Shared    float64 `json:"shared"`
		NotShared float64 `json:"notShared"`
	}
	if err := p.c.Get(ctx, "/albums/statistics", &st); err != nil {
		return err
	}
	s.albums.owned, s.albums.shared, s.albums.notShared = st.Owned, st.Shared, st.NotShared
	s.albums.statsOK = true
	return nil
}

func (p *poller) collectSharedLinks(ctx context.Context, s *snapshot) error {
	var links []struct {
		Type      string  `json:"type"`
		ExpiresAt *string `json:"expiresAt"`
		Password  *string `json:"password"`
	}
	if err := p.c.Get(ctx, "/shared-links", &links); err != nil {
		return err
	}
	sl := &s.albums.sharedLinks
	sl.ok = true
	now := time.Now()
	for _, l := range links {
		sl.byType[l.Type]++
		if l.ExpiresAt == nil {
			sl.neverExpire++
		} else if t, err := time.Parse(time.RFC3339, *l.ExpiresAt); err == nil && t.Before(now) {
			sl.expired++
		}
		if l.Password != nil && *l.Password != "" {
			sl.passwordProtected++
		}
	}
	return nil
}

func (p *poller) collectPartners(ctx context.Context, s *snapshot) error {
	dirs := map[string]string{"shared-by": "outgoing", "shared-with": "incoming"}
	for apiDir, label := range dirs {
		var arr []struct{}
		if err := p.c.Get(ctx, "/partners?direction="+apiDir, &arr); err != nil {
			return err
		}
		s.albums.partners[label] = float64(len(arr))
	}
	return nil
}

func (p *poller) collectTags(ctx context.Context, s *snapshot) error {
	var tags []struct {
		ParentID *string `json:"parentId"`
	}
	if err := p.c.Get(ctx, "/tags", &tags); err != nil {
		return err
	}
	s.content.tagsOK = true
	s.content.tags = float64(len(tags))
	for _, t := range tags {
		if t.ParentID == nil {
			s.content.tagsRoot++
		}
	}
	return nil
}

func (p *poller) collectMemories(ctx context.Context, s *snapshot) error {
	var st struct {
		Total float64 `json:"total"`
	}
	if err := p.c.Get(ctx, "/memories/statistics", &st); err != nil {
		return err
	}
	s.content.memories = set(st.Total)
	return nil
}

func (p *poller) collectLibraries(ctx context.Context, s *snapshot) error {
	var libs []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := p.c.Get(ctx, "/libraries", &libs); err != nil {
		return err
	}
	s.content.libraries = set(float64(len(libs)))
	for _, l := range libs {
		// The list's assetCount is always 0 on v3; only /statistics is authoritative.
		var st struct {
			Total float64 `json:"total"`
		}
		if err := p.c.Get(ctx, "/libraries/"+l.ID+"/statistics", &st); err != nil {
			if isSoftStatus(err) {
				continue // key lacks library.statistics: leave the sample absent, never a false zero
			}
			return err
		}
		s.content.perLibrary = append(s.content.perLibrary, labeledVal{id: l.ID, name: l.Name, value: st.Total})
	}
	return nil
}

func (p *poller) collectAPIKeys(ctx context.Context, s *snapshot) error {
	var keys []struct{}
	if err := p.c.Get(ctx, "/api-keys", &keys); err != nil {
		return err
	}
	s.content.apiKeys = set(float64(len(keys)))
	return nil
}

func (p *poller) collectSessions(ctx context.Context, s *snapshot) error {
	var sess []struct{}
	if err := p.c.Get(ctx, "/sessions", &sess); err != nil {
		return err
	}
	s.content.sessions = set(float64(len(sess)))
	return nil
}

func (p *poller) collectNotifications(ctx context.Context, s *snapshot) error {
	var notifs []struct {
		Level  string  `json:"level"`
		ReadAt *string `json:"readAt"`
	}
	if err := p.c.Get(ctx, "/notifications", &notifs); err != nil {
		return err
	}
	s.content.notifOK = true
	for _, n := range notifs {
		if n.ReadAt == nil {
			s.content.notifUnread++
		}
		if n.Level != "" {
			s.content.notifByLevel[n.Level]++
		}
	}
	return nil
}

func (p *poller) collectDuplicates(ctx context.Context, s *snapshot) error {
	var dups []struct {
		Assets []struct{} `json:"assets"`
	}
	if err := p.c.Get(ctx, "/duplicates", &dups); err != nil {
		return err
	}
	s.content.duplicatesOK = true
	s.content.duplicateSets = float64(len(dups))
	for _, d := range dups {
		s.content.duplicateAssets += float64(len(d.Assets))
	}
	return nil
}

func (p *poller) collectStacks(ctx context.Context, s *snapshot) error {
	var stacks []struct {
		Assets []struct{} `json:"assets"`
	}
	if err := p.c.Get(ctx, "/stacks", &stacks); err != nil {
		return err
	}
	s.content.stacksOK = true
	s.content.stacks = float64(len(stacks))
	for _, st := range stacks {
		s.content.stackedAssets += float64(len(st.Assets))
	}
	return nil
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

// parseSemver reads "v1.2.3[-pre][+build]" into numeric core fields plus a
// pre-release flag (build metadata never affects precedence). ok=false means
// the comparison is unknowable, and callers must not report an update rather
// than risk a false positive.
func parseSemver(v string) (parts [3]int, pre, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "+")
	v, _, pre = strings.Cut(v, "-")
	fields := strings.Split(v, ".")
	if len(fields) == 0 || len(fields) > 3 || fields[0] == "" {
		return parts, pre, false
	}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return parts, pre, false
		}
		parts[i] = n
	}
	return parts, pre, true
}

func semverGreater(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
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
