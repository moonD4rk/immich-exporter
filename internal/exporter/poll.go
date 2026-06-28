package exporter

import (
	"context"
	"log/slog"
	"sort"
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
	cameraMakes, cameraModels, lenses          float64
	hasCameraMakes, hasCameraModels, hasLenses bool
	assetsByMake, assetsByModel, assetsByLens  map[string]float64
	personAssets                               []labeledVal
}

type personRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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
		if err == nil || immich.StatusIs(err, 401, 403, 404) {
			return
		}
		p.scrapeErrors.Add(1)
		slog.Warn("optional collect failed", "section", name, "err", err)
	}

	s.serverUp = boolf(p.serverUp(ctx))

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

	hard("people", p.collectPeople(ctx, s))

	// Slow tier: refresh expensive fan-outs only every breakdownInterval and
	// carry the last result forward into this snapshot.
	if p.bd == nil || time.Since(p.lastBreakdown) >= p.cfg.BreakdownInterval {
		p.bd = p.collectBreakdowns(ctx, soft)
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

	s.scrapeDurationSeconds = time.Since(start).Seconds()
	if hardErrs == 0 {
		s.scrapeSuccess = 1
		s.lastSuccessUnix = float64(time.Now().Unix())
	} else if prev := p.snap.Load(); prev != nil {
		s.lastSuccessUnix = prev.lastSuccessUnix // preserve last good timestamp
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
	s.keyIsAdmin = boolf(me.IsAdmin)
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
	s.serverInfo = map[string]string{
		"version": a.Version, "source_ref": a.SourceRef, "source_commit": a.SourceCommit,
		"build": a.Build, "nodejs": a.Nodejs, "exiftool": a.Exiftool, "ffmpeg": a.Ffmpeg,
		"imagemagick": a.Imagemagick, "libvips": a.Libvips,
	}
	s.serverLicensed = boolf(a.Licensed)
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
	s.latestVersion = vc.ReleaseVersion
	running := s.serverInfo["version"]
	if running != "" && semver(vc.ReleaseVersion) != semver(running) {
		s.updateAvail = 1
	}
	return nil
}

func (p *poller) collectFeatures(ctx context.Context, s *snapshot) error {
	var f map[string]bool
	if err := p.c.Get(ctx, "/server/features", &f); err != nil {
		return err
	}
	for k, v := range f {
		s.features[camelToSnake(k)] = boolf(v)
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
		s.configTrashDays, s.hasConfigTrashDays = *c.TrashDays, true
	}
	if c.UserDeleteDelay != nil {
		s.configUserDeleteDelayDays, s.hasConfigDeleteDelay = *c.UserDeleteDelay, true
	}
	if c.MinFaces != nil {
		s.configMinFaces, s.hasConfigMinFaces = *c.MinFaces, true
	}
	if c.IsInitialized != nil {
		s.serverInitialized, s.hasInitialized = boolf(*c.IsInitialized), true
	}
	if c.IsOnboarded != nil {
		s.serverOnboarded, s.hasOnboarded = boolf(*c.IsOnboarded), true
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
	s.storageSizeBytes, s.storageUsedBytes, s.storageAvailableBytes = st.DiskSizeRaw, st.DiskUseRaw, st.DiskAvailableRaw
	s.hasStorage = true
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
		s.assets[t] = n
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
	s.assets["IMAGE"], s.assets["VIDEO"] = st.Photos, st.Videos
	s.assetStorage["IMAGE"], s.assetStorage["VIDEO"] = st.UsagePhotos, st.UsageVideos
	s.serverUsageBytes, s.hasServerUsage = st.Usage, true
	for _, u := range st.UsageByUser {
		us := userStat{id: u.UserID, name: u.UserName, photos: u.Photos, videos: u.Videos, usageBytes: u.Usage}
		if u.QuotaSizeInBytes == nil {
			us.quotaUnlimited = true
		} else {
			us.quotaBytes = *u.QuotaSizeInBytes
		}
		s.perUser = append(s.perUser, us)
	}
	return true, nil
}

func (p *poller) collectAssetStates(ctx context.Context, s *snapshot) error {
	type q struct {
		v      *float64
		ok     *bool
		filter map[string]any
	}
	checks := []q{
		{&s.assetsFavorite, &s.hasFavorite, map[string]any{"isFavorite": true}},
		{&s.assetsArchived, &s.hasArchived, map[string]any{"visibility": "archive"}},
		{&s.assetsHidden, &s.hasHidden, map[string]any{"visibility": "hidden"}},
		{&s.assetsLocked, &s.hasLocked, map[string]any{"visibility": "locked"}},
		{&s.assetsOffline, &s.hasOffline, map[string]any{"isOffline": true}},
		{&s.assetsMotion, &s.hasMotion, map[string]any{"isMotion": true}},
		{&s.assetsNotInAlbum, &s.hasNotInAlbum, map[string]any{"isNotInAlbum": true}},
		{&s.assetsEncoded, &s.hasEncoded, map[string]any{"isEncoded": true}},
	}
	for _, c := range checks {
		n, err := p.c.StatTotal(ctx, c.filter)
		if err != nil {
			return err
		}
		*c.v, *c.ok = n, true
	}
	// Trashed has no isTrashed boolean in StatisticsSearchDto; use /assets/statistics.
	var ts struct {
		Total float64 `json:"total"`
	}
	if err := p.c.Get(ctx, "/assets/statistics?isTrashed=true", &ts); err == nil {
		s.assetsTrashed, s.hasTrashed = ts.Total, true
	}
	return nil
}

func (p *poller) collectRatings(ctx context.Context, s *snapshot) error {
	for r := 1; r <= 5; r++ {
		n, err := p.c.StatTotal(ctx, map[string]any{"rating": r})
		if err != nil {
			return err
		}
		s.assetsByRating[strconv.Itoa(r)] = n
	}
	n, err := p.c.StatTotal(ctx, map[string]any{"rating": nil})
	if err != nil {
		return err
	}
	s.assetsByRating["unrated"] = n
	return nil
}

func (p *poller) collectYears(ctx context.Context, s *snapshot) error {
	var buckets []struct {
		TimeBucket string  `json:"timeBucket"`
		Count      float64 `json:"count"`
	}
	if err := p.c.Get(ctx, "/timeline/buckets?size=MONTH", &buckets); err != nil {
		return err
	}
	for _, b := range buckets {
		if len(b.TimeBucket) >= 4 {
			s.assetsByYear[b.TimeBucket[:4]] += b.Count
		}
	}
	return nil
}

// collectBreakdowns runs the expensive fan-out collectors into a fresh
// breakdownData. A failed sub-collector leaves its section empty (consistent
// with the "no stale values on failure" policy); between refreshes the last
// good result is carried forward by the caller.
func (p *poller) collectBreakdowns(ctx context.Context, soft func(string, error)) *breakdownData {
	bd := &breakdownData{}
	if p.cfg.CollectCamera {
		soft("cameras", p.collectCameras(ctx, bd))
	}
	if p.cfg.CollectPeople {
		soft("people/stats", p.collectPersonStats(ctx, bd))
	}
	return bd
}

func applyBreakdown(s *snapshot, bd *breakdownData) {
	if bd == nil {
		return
	}
	s.cameraMakes, s.hasCameraMakes = bd.cameraMakes, bd.hasCameraMakes
	s.cameraModels, s.hasCameraModels = bd.cameraModels, bd.hasCameraModels
	s.lenses, s.hasLenses = bd.lenses, bd.hasLenses
	s.assetsByMake, s.assetsByModel, s.assetsByLens = bd.assetsByMake, bd.assetsByModel, bd.assetsByLens
	s.personAssets = bd.personAssets
}

func (p *poller) collectCameras(ctx context.Context, bd *breakdownData) error {
	makes, err := p.c.Suggest(ctx, "camera-make")
	if err != nil {
		return err
	}
	bd.cameraMakes, bd.hasCameraMakes = float64(len(makes)), true
	models, err := p.c.Suggest(ctx, "camera-model")
	if err != nil {
		return err
	}
	bd.cameraModels, bd.hasCameraModels = float64(len(models)), true
	lenses, err := p.c.Suggest(ctx, "camera-lens-model")
	if err != nil {
		return err
	}
	bd.lenses, bd.hasLenses = float64(len(lenses)), true

	bd.assetsByMake = p.fanout(ctx, makes, "make", 0) // makes are low-cardinality: emit all
	bd.assetsByModel = p.fanout(ctx, models, "model", p.cfg.TopN)
	bd.assetsByLens = p.fanout(ctx, lenses, "lensModel", p.cfg.TopN)
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
	sem := make(chan struct{}, p.cfg.FanoutConcurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	m := make(map[string]float64, len(values))
	for _, v := range values {
		wg.Add(1)
		sem <- struct{}{}
		go func(v string) {
			defer wg.Done()
			defer func() { <-sem }()
			c, err := p.c.StatTotal(ctx, map[string]any{field: v})
			if err != nil {
				slog.Warn("fanout call failed", "field", field, "value", v, "err", err)
				return
			}
			mu.Lock()
			m[v] = c
			mu.Unlock()
		}(v)
	}
	wg.Wait()
	return topNWithOther(m, n)
}

func (p *poller) collectGeo(ctx context.Context, s *snapshot) error {
	if cities, err := p.c.Suggest(ctx, "city"); err == nil {
		s.cities, s.hasCities = float64(len(cities)), true
	}
	if states, err := p.c.Suggest(ctx, "state"); err == nil {
		s.states, s.hasStates = float64(len(states)), true
	}
	if countries, err := p.c.Suggest(ctx, "country"); err == nil {
		s.countries, s.hasCountries = float64(len(countries)), true
	}
	// One /map/markers call yields the geotagged total, the per-country split, and
	// each country's asset centroid (mean lat/lon) for a coords-mode geomap — no
	// country-name → ISO mapping required.
	var markers []struct {
		Country string  `json:"country"`
		Lat     float64 `json:"lat"`
		Lon     float64 `json:"lon"`
	}
	if err := p.c.Get(ctx, "/map/markers", &markers); err != nil {
		return err
	}
	s.geotagged, s.hasGeotagged = float64(len(markers)), true
	type acc struct{ sumLat, sumLon, count float64 }
	agg := map[string]*acc{}
	for _, m := range markers {
		country := strings.TrimSpace(m.Country)
		if country == "" {
			country = "unknown"
			s.assetsByCountry[country]++
			continue
		}
		s.assetsByCountry[country]++
		a := agg[country]
		if a == nil {
			a = &acc{}
			agg[country] = a
		}
		a.sumLat, a.sumLon, a.count = a.sumLat+m.Lat, a.sumLon+m.Lon, a.count+1
	}
	for country, a := range agg {
		// Round to ~11 km so the centroid label stays stable as assets are added.
		lat := strconv.FormatFloat(a.sumLat/a.count, 'f', 1, 64)
		lon := strconv.FormatFloat(a.sumLon/a.count, 'f', 1, 64)
		s.geoCentroids[country] = [2]string{lat, lon}
	}
	return nil
}

func (p *poller) collectPeople(ctx context.Context, s *snapshot) error {
	var named, withBirthdate float64
	page := 1
	for {
		var resp struct {
			People []struct {
				Name      string  `json:"name"`
				BirthDate *string `json:"birthDate"`
			} `json:"people"`
			HasNextPage bool    `json:"hasNextPage"`
			Total       float64 `json:"total"`
			Hidden      float64 `json:"hidden"`
		}
		if err := p.c.Get(ctx, "/people?withHidden=true&size=1000&page="+strconv.Itoa(page), &resp); err != nil {
			return err
		}
		if page == 1 {
			s.people, s.peopleHidden = resp.Total, resp.Hidden
			s.hasPeople = true
		}
		for _, pe := range resp.People {
			if strings.TrimSpace(pe.Name) != "" {
				named++
			}
			if pe.BirthDate != nil {
				withBirthdate++
			}
		}
		if !resp.HasNextPage || len(resp.People) == 0 {
			break
		}
		page++
	}
	s.peopleNamed = named
	s.peopleUnnamed = s.people - named
	s.peopleWithBirthdate = withBirthdate
	return nil
}

func (p *poller) collectPersonStats(ctx context.Context, bd *breakdownData) error {
	var ids []personRef
	page := 1
	for len(ids) < p.cfg.FanoutLimit {
		var resp struct {
			People      []personRef `json:"people"`
			HasNextPage bool        `json:"hasNextPage"`
		}
		if err := p.c.Get(ctx, "/people?withHidden=true&size=1000&page="+strconv.Itoa(page), &resp); err != nil {
			return err
		}
		ids = append(ids, resp.People...)
		if !resp.HasNextPage || len(resp.People) == 0 {
			break
		}
		page++
	}
	// Cap to FanoutLimit: a full last page can overshoot it (the camera fanout() helper enforces the same bound).
	if len(ids) > p.cfg.FanoutLimit {
		ids = ids[:p.cfg.FanoutLimit]
	}
	sem := make(chan struct{}, p.cfg.FanoutConcurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	vals := make([]labeledVal, 0, len(ids))
	for _, pe := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(pe personRef) {
			defer wg.Done()
			defer func() { <-sem }()
			var st struct {
				Assets float64 `json:"assets"`
			}
			if err := p.c.Get(ctx, "/people/"+pe.ID+"/statistics", &st); err != nil {
				return
			}
			name := pe.Name
			if name == "" {
				name = "(unnamed)"
			}
			mu.Lock()
			vals = append(vals, labeledVal{id: pe.ID, name: name, value: st.Assets})
			mu.Unlock()
		}(pe)
	}
	wg.Wait()
	bd.personAssets = topNLabeled(vals, p.cfg.TopN)
	return nil
}

func (p *poller) collectUsers(ctx context.Context, s *snapshot) error {
	var users []struct {
		Status  string `json:"status"`
		IsAdmin bool   `json:"isAdmin"`
	}
	if err := p.c.Get(ctx, "/admin/users?withDeleted=true", &users); err != nil {
		return err
	}
	s.hasUsers = true
	for _, u := range users {
		st := u.Status
		if st == "" {
			st = "active"
		}
		s.usersByStatus[st]++
		if u.IsAdmin {
			s.usersByRole["admin"]++
		} else {
			s.usersByRole["user"]++
		}
	}
	return nil
}

func (p *poller) collectJobs(ctx context.Context, s *snapshot) error {
	var jobs map[string]struct {
		JobCounts struct {
			Active    float64 `json:"active"`
			Completed float64 `json:"completed"`
			Failed    float64 `json:"failed"`
			Delayed   float64 `json:"delayed"`
			Waiting   float64 `json:"waiting"`
			Paused    float64 `json:"paused"`
		} `json:"jobCounts"`
		QueueStatus struct {
			IsPaused bool `json:"isPaused"`
		} `json:"queueStatus"`
	}
	if err := p.c.Get(ctx, "/jobs", &jobs); err != nil {
		return err
	}
	for q, j := range jobs {
		jc := j.JobCounts
		s.jobQueues = append(s.jobQueues,
			jobQueueStat{q, "active", jc.Active},
			jobQueueStat{q, "completed", jc.Completed},
			jobQueueStat{q, "failed", jc.Failed},
			jobQueueStat{q, "delayed", jc.Delayed},
			jobQueueStat{q, "waiting", jc.Waiting},
			jobQueueStat{q, "paused", jc.Paused},
		)
		s.jobQueuePaused[q] = boolf(j.QueueStatus.IsPaused)
	}
	return nil
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
	s.hasAlbums = true
	var sum, maxCount float64
	vals := make([]labeledVal, 0, len(albums))
	for _, a := range albums {
		if a.Shared {
			s.albumsSharedCount++
		} else {
			s.albumsPrivateCount++
		}
		sum += a.AssetCount
		if a.AssetCount > maxCount {
			maxCount = a.AssetCount
		}
		if a.AssetCount == 0 {
			s.albumsEmpty++
		}
		if a.HasSharedLink {
			s.albumsWithSharedLink++
		}
		vals = append(vals, labeledVal{id: a.ID, name: a.AlbumName, value: a.AssetCount})
	}
	s.albumAssets, s.albumAssetsMax = sum, maxCount
	if n := len(albums); n > 0 {
		s.albumAssetsAvg = sum / float64(n)
	}
	s.topAlbums = topNLabeled(vals, p.cfg.TopN)
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
	s.albumsOwned, s.albumsShared, s.albumsNotShared = st.Owned, st.Shared, st.NotShared
	s.hasAlbumStats = true
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
	s.hasSharedLinks = true
	now := time.Now()
	for _, l := range links {
		s.sharedLinks[l.Type]++
		if l.ExpiresAt == nil {
			s.sharedLinksNeverExpire++
		} else if t, err := time.Parse(time.RFC3339, *l.ExpiresAt); err == nil && t.Before(now) {
			s.sharedLinksExpired++
		}
		if l.Password != nil && *l.Password != "" {
			s.sharedLinksPasswordProtected++
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
		s.partners[label] = float64(len(arr))
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
	s.hasTags = true
	s.tags = float64(len(tags))
	for _, t := range tags {
		if t.ParentID == nil {
			s.tagsRoot++
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
	s.memories, s.hasMemories = st.Total, true
	return nil
}

func (p *poller) collectLibraries(ctx context.Context, s *snapshot) error {
	var libs []struct {
		ID         string  `json:"id"`
		Name       string  `json:"name"`
		AssetCount float64 `json:"assetCount"`
	}
	if err := p.c.Get(ctx, "/libraries", &libs); err != nil {
		return err
	}
	s.libraries, s.hasLibraries = float64(len(libs)), true
	for _, l := range libs {
		s.perLibrary = append(s.perLibrary, labeledVal{id: l.ID, name: l.Name, value: l.AssetCount})
	}
	return nil
}

func (p *poller) collectAPIKeys(ctx context.Context, s *snapshot) error {
	var keys []struct{}
	if err := p.c.Get(ctx, "/api-keys", &keys); err != nil {
		return err
	}
	s.apiKeys, s.hasAPIKeys = float64(len(keys)), true
	return nil
}

func (p *poller) collectSessions(ctx context.Context, s *snapshot) error {
	var sess []struct{}
	if err := p.c.Get(ctx, "/sessions", &sess); err != nil {
		return err
	}
	s.sessions, s.hasSessions = float64(len(sess)), true
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
	s.hasNotifications = true
	for _, n := range notifs {
		if n.ReadAt == nil {
			s.notificationsUnread++
		}
		if n.Level != "" {
			s.notificationsByLevel[n.Level]++
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
	s.hasDuplicates = true
	s.duplicateSets = float64(len(dups))
	for _, d := range dups {
		s.duplicateAssets += float64(len(d.Assets))
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
	s.hasStacks = true
	s.stacks = float64(len(stacks))
	for _, st := range stacks {
		s.stackedAssets += float64(len(st.Assets))
	}
	return nil
}

// --- helpers ---

func boolf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func semver(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

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
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].v != arr[j].v {
			return arr[i].v > arr[j].v
		}
		return arr[i].k < arr[j].k
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
	sort.Slice(vals, func(i, j int) bool {
		if vals[i].value != vals[j].value {
			return vals[i].value > vals[j].value
		}
		return vals[i].id < vals[j].id
	})
	if n > 0 && len(vals) > n {
		vals = vals[:n]
	}
	return vals
}
