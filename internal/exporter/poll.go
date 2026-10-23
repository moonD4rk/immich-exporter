package exporter

import (
	"cmp"
	"context"
	"iter"
	"log/slog"
	"maps"
	"regexp"
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

func (p *poller) serverUp(ctx context.Context) bool {
	var out struct {
		Res string `json:"res"`
	}
	return p.c.Get(ctx, "/server/ping", &out) == nil && out.Res == "pong"
}

// collectMaintenance reads maintenance mode (Immich 2.5+), which needs no permission.
func (p *poller) collectMaintenance(ctx context.Context, s *snapshot) error {
	var st struct {
		Active bool `json:"active"`
	}
	if err := p.c.Get(ctx, "/admin/maintenance/status", &st); err != nil {
		return err
	}
	s.server.maintenance = set(boolf(st.Active))
	return nil
}

func (p *poller) collectMe(ctx context.Context, s *snapshot) error {
	var me struct {
		IsAdmin bool `json:"isAdmin"`
	}
	if err := p.c.Get(ctx, "/users/me", &me); err != nil {
		return err
	}
	s.health.keyIsAdmin = boolf(me.IsAdmin)
	return nil
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

// versionCheckMaxAge bounds how old Immich's last upstream check may be before
// its answer is withheld. Disabling the check freezes the stored answer, which
// kept reporting v2.5.6 for months to a server long since on v3.
const versionCheckMaxAge = 7 * 24 * time.Hour

func (p *poller) collectVersionCheck(ctx context.Context, s *snapshot) error {
	var vc struct {
		ReleaseVersion string  `json:"releaseVersion"`
		CheckedAt      *string `json:"checkedAt"`
	}
	if err := p.c.Get(ctx, "/server/version-check", &vc); err != nil {
		return err
	}
	if vc.CheckedAt != nil {
		if t, err := time.Parse(time.RFC3339, *vc.CheckedAt); err == nil {
			s.server.versionCheckedUnix = set(float64(t.Unix()))
			if time.Since(t) > versionCheckMaxAge {
				return nil
			}
		}
	}
	if vc.ReleaseVersion == "" {
		return nil
	}
	release, relPre, relOK := parseSemver(vc.ReleaseVersion)
	running, runPre, runOK := parseSemver(s.server.about.version)
	if relOK && runOK && semverGreater(running, release) {
		return nil // a release older than the server: the check predates the last upgrade
	}
	s.server.latestVersion = vc.ReleaseVersion
	// A stable release also supersedes a running pre-release of the same core version.
	if relOK && runOK && (semverGreater(release, running) || (release == running && runPre && !relPre)) {
		s.server.updateAvail = 1
	}
	return nil
}

// collectVersionHistory finds when the running version first started, which
// needs no permission.
func (p *poller) collectVersionHistory(ctx context.Context, s *snapshot) error {
	var history []struct {
		Version   string `json:"version"`
		CreatedAt string `json:"createdAt"`
	}
	if err := p.c.Get(ctx, "/server/version-history", &history); err != nil {
		return err
	}
	running := strings.TrimPrefix(s.server.about.version, "v")
	var since time.Time
	for _, h := range history {
		t, err := time.Parse(time.RFC3339, h.CreatedAt)
		// The latest entry wins: a version reinstalled after a downgrade has been running since then.
		if err == nil && running != "" && strings.TrimPrefix(h.Version, "v") == running && t.After(since) {
			since = t
		}
	}
	if !since.IsZero() {
		s.server.versionInstalledUnix = set(float64(since.Unix()))
	}
	return nil
}

// collectFeatures reads /server/features, deprecated since Immich 3.2, and
// falls back to /config once a server drops it.
func (p *poller) collectFeatures(ctx context.Context, s *snapshot) error {
	var f map[string]bool
	err := p.c.Get(ctx, "/server/features", &f)
	if immich.StatusIs(err, 404) {
		return p.collectFeaturesFromConfig(ctx, s)
	}
	if err != nil {
		return err
	}
	for k, v := range f {
		s.server.features[camelToSnake(k)] = boolf(v)
	}
	return nil
}

type toggle struct {
	Enabled bool `json:"enabled"`
}

// userConfig is the part of GET /config (Immich 3.2+) that replaces /server/features and /server/config.
type userConfig struct {
	FFmpeg struct {
		Realtime toggle `json:"realtime"`
	} `json:"ffmpeg"`
	MachineLearning struct {
		Enabled            bool   `json:"enabled"`
		Clip               toggle `json:"clip"`
		DuplicateDetection toggle `json:"duplicateDetection"`
		Ocr                toggle `json:"ocr"`
		FacialRecognition  struct {
			Enabled  bool     `json:"enabled"`
			MinFaces *float64 `json:"minFaces"`
		} `json:"facialRecognition"`
	} `json:"machineLearning"`
	Map              toggle `json:"map"`
	ReverseGeocoding toggle `json:"reverseGeocoding"`
	OAuth            struct {
		Enabled    bool `json:"enabled"`
		AutoLaunch bool `json:"autoLaunch"`
	} `json:"oauth"`
	PasswordLogin toggle `json:"passwordLogin"`
	Trash         struct {
		Enabled bool     `json:"enabled"`
		Days    *float64 `json:"days"`
	} `json:"trash"`
	User struct {
		DeleteDelay *float64 `json:"deleteDelay"`
	} `json:"user"`
}

// collectFeaturesFromConfig derives the feature flags Immich computes for
// /server/features. /config has no counterpart for config_file, email,
// import_faces, search or sidecar, so those go absent.
func (p *poller) collectFeaturesFromConfig(ctx context.Context, s *snapshot) error {
	var c userConfig
	if err := p.c.Get(ctx, "/config", &c); err != nil {
		return err
	}
	ml := c.MachineLearning
	for name, on := range map[string]bool{
		"smart_search":         ml.Enabled && ml.Clip.Enabled,
		"facial_recognition":   ml.Enabled && ml.FacialRecognition.Enabled,
		"duplicate_detection":  ml.Enabled && ml.DuplicateDetection.Enabled,
		"ocr":                  ml.Enabled && ml.Ocr.Enabled,
		"map":                  c.Map.Enabled,
		"reverse_geocoding":    c.ReverseGeocoding.Enabled,
		"oauth":                c.OAuth.Enabled,
		"oauth_auto_launch":    c.OAuth.AutoLaunch,
		"password_login":       c.PasswordLogin.Enabled,
		"realtime_transcoding": c.FFmpeg.Realtime.Enabled,
		"trash":                c.Trash.Enabled,
	} {
		s.server.features[name] = boolf(on)
	}
	return nil
}

// collectConfig reads /server/config, deprecated since Immich 3.2, and falls
// back to /config once a server drops it; /config has no initialized or
// onboarded flag, so those go absent then.
func (p *poller) collectConfig(ctx context.Context, s *snapshot) error {
	var c struct {
		TrashDays       *float64 `json:"trashDays"`
		UserDeleteDelay *float64 `json:"userDeleteDelay"`
		MinFaces        *float64 `json:"minFaces"`
		IsInitialized   *bool    `json:"isInitialized"`
		IsOnboarded     *bool    `json:"isOnboarded"`
	}
	err := p.c.Get(ctx, "/server/config", &c)
	switch {
	case immich.StatusIs(err, 404):
		return p.collectConfigFromUserConfig(ctx, s)
	case err != nil:
		return err
	}
	sv := &s.server
	sv.trashDays, sv.userDeleteDelayDays, sv.minFaces = optional(c.TrashDays), optional(c.UserDeleteDelay), optional(c.MinFaces)
	if c.IsInitialized != nil {
		sv.initialized = set(boolf(*c.IsInitialized))
	}
	if c.IsOnboarded != nil {
		sv.onboarded = set(boolf(*c.IsOnboarded))
	}
	return nil
}

func (p *poller) collectConfigFromUserConfig(ctx context.Context, s *snapshot) error {
	var c userConfig
	if err := p.c.Get(ctx, "/config", &c); err != nil {
		return err
	}
	sv := &s.server
	sv.trashDays, sv.userDeleteDelayDays = optional(c.Trash.Days), optional(c.User.DeleteDelay)
	sv.minFaces = optional(c.MachineLearning.FacialRecognition.MinFaces)
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
		UsagePhotos      *float64 `json:"usagePhotos"`
		UsageVideos      *float64 `json:"usageVideos"`
		QuotaSizeInBytes *float64 `json:"quotaSizeInBytes"`
	} `json:"usageByUser"`
}

func optional(v *float64) opt {
	if v == nil {
		return opt{}
	}
	return set(*v)
}

func (p *poller) collectAssets(ctx context.Context, s *snapshot) error {
	if s.health.keyIsAdmin == 1 {
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
		us := userStat{
			id: u.UserID, name: u.UserName, photos: u.Photos, videos: u.Videos, usageBytes: u.Usage,
			photoBytes: optional(u.UsagePhotos), videoBytes: optional(u.UsageVideos),
		}
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
		{&a.offline, map[string]any{"isOffline": true}},
		{&a.motion, map[string]any{"isMotion": true}},
		{&a.notInAlbum, map[string]any{"isNotInAlbum": true}},
		{&a.encoded, map[string]any{"isEncoded": true}},
	}
	// A filter the API key may not query must not drop the other states, but
	// refusing all of them is a failure, not an empty success.
	var refused error
	for _, c := range checks {
		n, err := p.c.StatTotal(ctx, c.filter)
		if err != nil {
			if isSoftStatus(err) {
				refused = err
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
	switch err := p.c.Get(ctx, "/assets/statistics?isTrashed=true", &ts); {
	case err == nil:
		a.trashed = set(ts.Total)
	case !isSoftStatus(err):
		return err
	default:
		refused = err
	}
	for _, v := range []opt{a.favorite, a.archived, a.hidden, a.offline, a.motion, a.notInAlbum, a.encoded, a.trashed} {
		if v.ok {
			return nil
		}
	}
	return refused
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

// searchFilter reports whether the server reads StatisticsSearchDto.filter
// (Immich 3.2+). An older one strips the unknown field and counts every asset.
func searchFilter(version string) bool {
	v, _, ok := parseSemver(version)
	return ok && !semverGreater([3]int{3, 2, 0}, v)
}

// visible wraps a SearchFilter into a StatisticsSearchDto body that counts what
// users browse, the timeline and the archive. That leaves out the hidden video
// halves of live photos, which search otherwise counts as assets of their own
// (a library of 64k assets searched as 75k), and the PIN-locked folder, which
// an API key may not search at all.
func visible(filter map[string]any) map[string]any {
	f := maps.Clone(filter)
	f["visibility"] = map[string]any{"in": []string{"timeline", "archive"}}
	return map[string]any{"filter": f}
}

// sizeBuckets bound the file-size buckets of immich_assets_by_file_size.
var sizeBuckets = []struct {
	name  string
	below float64
}{
	{"0-1MiB", 1 << 20},
	{"1-5MiB", 5 << 20},
	{"5-20MiB", 20 << 20},
	{"20-100MiB", 100 << 20},
	{"100MiB-1GiB", 1 << 30},
}

// collectMetadata counts what only SearchFilter can express among visible
// assets: those showing recognized people, those with camera EXIF, and all of
// them by file size. Assets without a recorded size land in "unknown" rather
// than in the top bucket.
func (p *poller) collectMetadata(ctx context.Context, s *snapshot) error {
	m := &s.metadata
	count := func(filter map[string]any) (float64, error) { return p.c.StatTotal(ctx, visible(filter)) }
	n, err := count(map[string]any{"hasPeople": map[string]any{"eq": true}})
	if err != nil {
		return err
	}
	m.withPeople = set(n)
	if n, err = count(map[string]any{"make": map[string]any{"ne": nil}}); err != nil {
		return err
	}
	m.withCamera = set(n)

	total, err := count(map[string]any{})
	if err != nil {
		return err
	}
	var below float64
	for _, b := range sizeBuckets {
		if n, err = count(map[string]any{"fileSizeInBytes": map[string]any{"lt": b.below}}); err != nil {
			return err
		}
		m.bySize[b.name] = n - below
		below = n
	}
	top := sizeBuckets[len(sizeBuckets)-1].below
	above, err := count(map[string]any{"fileSizeInBytes": map[string]any{"gte": top}})
	if err != nil {
		return err
	}
	m.bySize["1GiB+"] = above
	if unknown := total - below - above; unknown > 0 {
		m.bySize["unknown"] = unknown
	}
	return nil
}

func (p *poller) collectCameras(ctx context.Context, s *snapshot) error {
	makes, err := p.c.Suggest(ctx, "camera-make")
	if err != nil {
		return err
	}
	s.cameras.makes = set(float64(len(makes)))
	models, err := p.c.Suggest(ctx, "camera-model")
	if err != nil {
		return err
	}
	s.cameras.models = set(float64(len(models)))
	lenses, err := p.c.Suggest(ctx, "camera-lens-model")
	if err != nil {
		return err
	}
	s.cameras.lenses = set(float64(len(lenses)))

	s.cameras.byMake = p.fanout(ctx, makes, "make", 0) // makes are low-cardinality: emit all
	s.cameras.byModel = p.fanout(ctx, models, "model", p.cfg.TopN)
	s.cameras.byLens = p.fanout(ctx, lenses, "lensModel", p.cfg.TopN)
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
	foldCities(g, p.cfg.CityLimit)
	return nil
}

// foldCities keeps the n cities with the most assets and sums the rest into one
// "other" city without coordinates, bounding immich_assets_by_city for libraries
// that span thousands of places. n <= 0 keeps every city.
func foldCities(g *geoSnap, n int) {
	if n <= 0 || len(g.byCity) <= n {
		return
	}
	keys := slices.Collect(maps.Keys(g.byCity))
	slices.SortFunc(keys, func(a, b cityKey) int {
		return cmp.Or(cmp.Compare(g.byCity[b], g.byCity[a]), cmp.Compare(a.country, b.country), cmp.Compare(a.city, b.city))
	})
	other := cityKey{city: "other"}
	for _, k := range keys[n:] {
		g.byCity[other] += g.byCity[k]
		delete(g.byCity, k)
		delete(g.cityCentroids, k)
	}
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

// collectPersonStats fails only when every per-person call did, so one person
// the key can't read doesn't hide the rest.
func (p *poller) collectPersonStats(ctx context.Context, people []person, s *snapshot) error {
	// Cap to FanoutLimit, matching the bound fanout() enforces for cameras.
	people = people[:min(len(people), p.cfg.FanoutLimit)]
	var lastErr atomic.Pointer[error]
	vals := parallel(people, p.cfg.FanoutConcurrency, func(pe person) (labeledVal, bool) {
		var st struct {
			Assets float64 `json:"assets"`
		}
		if err := p.c.Get(ctx, "/people/"+pe.ID+"/statistics", &st); err != nil {
			lastErr.Store(&err)
			return labeledVal{}, false
		}
		name := pe.Name
		if name == "" {
			name = "(unnamed)"
		}
		return labeledVal{id: pe.ID, name: name, value: st.Assets}, true
	})
	if len(vals) == 0 && lastErr.Load() != nil {
		return *lastErr.Load()
	}
	s.people.assets = topNLabeled(vals, p.cfg.TopN)
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

// collectLocked sums every user's locked-folder assets. Search refuses the
// locked visibility to an API key ("Elevated permission is required"), but the
// admin per-user statistics answer it.
func (p *poller) collectLocked(ctx context.Context, s *snapshot) error {
	var users []struct {
		ID string `json:"id"`
	}
	if err := p.c.Get(ctx, "/admin/users", &users); err != nil {
		return err
	}
	var locked float64
	for _, u := range users {
		var st struct {
			Total float64 `json:"total"`
		}
		if err := p.c.Get(ctx, "/admin/users/"+u.ID+"/statistics?visibility=locked", &st); err != nil {
			return err
		}
		locked += st.Total
	}
	s.assets.locked = set(locked)
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

// collectIntegrity reads the files Immich's own integrity checks (Immich 3+) flagged on their latest run.
func (p *poller) collectIntegrity(ctx context.Context, s *snapshot) error {
	var summary map[string]float64
	if err := p.c.Get(ctx, "/admin/integrity/summary", &summary); err != nil {
		return err
	}
	maps.Copy(s.ops.integrity, summary)
	return nil
}

// backupStamp is the local time Immich writes into a backup's file name, e.g.
// immich-db-backup-20261022T020000-v3.3.3-pg18.6.sql.gz; an uploaded backup
// may carry a prefix before it.
var backupStamp = regexp.MustCompile(`immich-db-backup-(\d{8}T\d{6})-`)

// collectBackups lists the database backups Immich (2.5+) keeps.
func (p *poller) collectBackups(ctx context.Context, s *snapshot) error {
	var res struct {
		Backups []struct {
			Filename string  `json:"filename"`
			Filesize float64 `json:"filesize"`
			Timezone string  `json:"timezone"`
		} `json:"backups"`
	}
	if err := p.c.Get(ctx, "/admin/database-backups", &res); err != nil {
		return err
	}
	o := &s.ops
	o.backupsOK = true
	o.backups = float64(len(res.Backups))
	var newest time.Time
	for _, b := range res.Backups {
		o.backupsBytes += b.Filesize
		if t, ok := backupTime(b.Filename, b.Timezone); ok && t.After(newest) {
			newest = t
			o.lastBackupBytes = set(b.Filesize)
		}
	}
	if !newest.IsZero() {
		o.lastBackupUnix = set(float64(newest.Unix()))
	}
	return nil
}

// backupTime reads when Immich wrote a backup from its file name, stamped in
// the server's time zone; an unknown zone falls back to UTC.
func backupTime(name, zone string) (time.Time, bool) {
	m := backupStamp.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	t, err := time.ParseInLocation("20060102T150405", m[1], loc)
	return t, err == nil
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
	// Known types report 0 instead of vanishing, so a library without links totals 0 rather than no data.
	sl.byType["ALBUM"], sl.byType["INDIVIDUAL"] = 0, 0
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
			Total float64  `json:"total"`
			Usage *float64 `json:"usage"`
		}
		if err := p.c.Get(ctx, "/libraries/"+l.ID+"/statistics", &st); err != nil {
			if isSoftStatus(err) {
				continue // key lacks library.statistics: leave the sample absent, never a false zero
			}
			return err
		}
		s.content.perLibrary = append(s.content.perLibrary, labeledVal{id: l.ID, name: l.Name, value: st.Total})
		if st.Usage != nil {
			s.content.perLibraryBytes = append(s.content.perLibraryBytes, labeledVal{id: l.ID, name: l.Name, value: *st.Usage})
		}
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

// collectWorkflows counts the key owner's workflows (Immich 3+). Both states are
// always reported so a library without workflows shows 0 rather than no data.
func (p *poller) collectWorkflows(ctx context.Context, s *snapshot) error {
	var workflows []struct {
		Enabled bool `json:"enabled"`
	}
	if err := p.c.Get(ctx, "/workflows", &workflows); err != nil {
		return err
	}
	w := s.content.workflows
	w["true"], w["false"] = 0, 0
	for _, wf := range workflows {
		w[strconv.FormatBool(wf.Enabled)]++
	}
	return nil
}

func (p *poller) collectPlugins(ctx context.Context, s *snapshot) error {
	var plugins []struct{}
	if err := p.c.Get(ctx, "/plugins", &plugins); err != nil {
		return err
	}
	s.content.plugins = set(float64(len(plugins)))
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
