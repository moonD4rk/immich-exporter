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

// userStat holds the per-user volumetrics from /server/statistics.
type userStat struct {
	id, name       string
	photos, videos float64
	usageBytes     float64
	quotaBytes     float64
	quotaUnlimited bool
}

// cityKey identifies a city within a country; city names repeat across
// countries, so the pair is the aggregation unit.
type cityKey struct {
	city, country string
}

type latlon struct {
	lat, lon string
}

// jobQueueStat is one (queue,state) -> count datapoint plus the paused flag.
type jobQueueStat struct {
	queue, state string
	count        float64
}

// aboutInfo mirrors the /server/about fields exported as immich_server_info
// labels; a typed struct keeps the poller and the label order of dServerInfo
// aligned at compile time.
type aboutInfo struct {
	version, sourceRef, sourceCommit, build,
	nodejs, exiftool, ffmpeg, imagemagick, libvips string
	ok bool
}

type healthSnap struct {
	serverUp, scrapeSuccess, scrapeDurationSeconds, lastSuccessUnix, keyIsAdmin float64
}

type serverSnap struct {
	about         aboutInfo
	licensed      float64
	latestVersion string
	updateAvail   float64
	features      map[string]float64 // feature -> 0/1; keys change across Immich versions, so a map is deliberate
	trashDays, userDeleteDelayDays, minFaces,
	initialized, onboarded opt
	// storage of the volume backing UPLOAD_LOCATION; the three are set together
	storageSizeBytes, storageUsedBytes, storageAvailableBytes opt
}

type assetsSnap struct {
	byType           map[string]float64 // AssetTypeEnum -> count (instance-wide with admin, owner-scoped otherwise)
	storageByType    map[string]float64 // AssetTypeEnum -> bytes
	serverUsageBytes opt
	byYear           map[string]float64
	byRating         map[string]float64 // "1".."5","unrated"
	favorite, archived, hidden, locked, offline,
	motion, notInAlbum, encoded, trashed opt
}

type camerasSnap struct {
	makes, models, lenses   opt
	byMake, byModel, byLens map[string]float64
}

type geoSnap struct {
	cities, states, countries, geotagged opt
	byCountry                            map[string]float64 // country -> count
	countryCentroids                     map[string]latlon  // centroid of the country's own assets
	byCity                               map[cityKey]float64
	cityCentroids                        map[cityKey]latlon
}

type peopleSnap struct {
	total, hidden, named, unnamed, withBirthdate float64
	ok                                           bool
	assets                                       []labeledVal // per-person asset counts, top-N
}

type usersSnap struct {
	byStatus, byRole map[string]float64
	ok               bool
	perUser          []userStat
}

type sharedLinksSnap struct {
	byType                                  map[string]float64
	expired, neverExpire, passwordProtected float64
	ok                                      bool
}

type albumsSnap struct {
	owned, shared, notShared float64
	statsOK                  bool // /albums/statistics succeeded
	sharedCount, privateCount, assetsTotal, empty,
	withSharedLink, assetsMax, assetsAvg float64
	ok          bool // /albums succeeded
	top         []labeledVal
	sharedLinks sharedLinksSnap
	partners    map[string]float64 // direction -> count
}

type contentSnap struct {
	tags, tagsRoot                 float64
	tagsOK                         bool
	memories                       opt
	duplicateSets, duplicateAssets float64
	duplicatesOK                   bool
	stacks, stackedAssets          float64
	stacksOK                       bool
	libraries                      opt
	perLibrary                     []labeledVal
	apiKeys, sessions              opt
	notifUnread                    float64
	notifOK                        bool
	notifByLevel                   map[string]float64
}

type jobsSnap struct {
	queues []jobQueueStat
	paused map[string]float64
}

// snapshot is the complete set of values from one poll cycle, grouped by the
// same domains the emit functions use. The collector emits it verbatim on
// every Prometheus scrape, so a label combination that disappears upstream
// simply stops being emitted (no stale series).
type snapshot struct {
	health  healthSnap
	server  serverSnap
	assets  assetsSnap
	cameras camerasSnap
	geo     geoSnap
	people  peopleSnap
	users   usersSnap
	albums  albumsSnap
	content contentSnap
	jobs    jobsSnap
}

func newSnapshot() *snapshot {
	return &snapshot{
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
		users: usersSnap{byStatus: map[string]float64{}, byRole: map[string]float64{}},
		albums: albumsSnap{
			sharedLinks: sharedLinksSnap{byType: map[string]float64{}},
			partners:    map[string]float64{},
		},
		content: contentSnap{notifByLevel: map[string]float64{}},
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

	// server / system
	dServerUp        = desc("up", "1 if the Immich server answered /server/ping.")
	dServerInfo      = desc("server_info", "Immich server version and dependency metadata (value is always 1).", "version", "source_ref", "source_commit", "build", "nodejs", "exiftool", "ffmpeg", "imagemagick", "libvips")
	dServerLicensed  = desc("server_licensed", "1 if the Immich server has an active license.")
	dLatestVersion   = desc("server_latest_version_info", "Latest Immich release available upstream (value is always 1).", "release_version")
	dUpdateAvailable = desc("server_update_available", "1 if a newer Immich release is available upstream.")
	dFeature         = desc("server_feature", "Immich server feature flags (1 enabled, 0 disabled).", "feature")
	dConfigTrashDays = desc("config_trash_days", "Days assets stay in trash before automatic deletion.")
	dConfigDelDelay  = desc("config_user_delete_delay_days", "Grace period before a deleted user is purged, in days.")
	dConfigMinFaces  = desc("config_min_faces", "Minimum faces threshold for facial-recognition clustering.")
	dInitialized     = desc("server_initialized", "1 if the server has completed first-run initialization.")
	dOnboarded       = desc("server_onboarded", "1 if admin onboarding has completed.")

	// storage
	dStorageSize  = desc("storage_size_bytes", "Total capacity of the filesystem backing the upload location.")
	dStorageUsed  = desc("storage_used_bytes", "Used bytes on the upload filesystem (whole volume, not just Immich).")
	dStorageAvail = desc("storage_available_bytes", "Free bytes on the upload filesystem.")

	// assets
	dAssets       = desc("assets", "Number of assets by type.", "type")
	dAssetStorage = desc("assets_storage_bytes", "Storage used by assets by type, in bytes.", "type")
	dServerUsage  = desc("server_usage_bytes", "Total bytes used by all original assets server-wide (admin).")
	dAssetsByYear = desc("assets_by_year", "Assets taken per calendar year.", "year")
	dAssetsByRate = desc("assets_by_rating", "Assets by star rating (1-5, or 'unrated').", "rating")
	dFavorite     = desc("assets_favorite", "Number of favourited assets.")
	dArchived     = desc("assets_archived", "Number of archived assets.")
	dHidden       = desc("assets_hidden", "Number of hidden assets.")
	dLocked       = desc("assets_locked", "Number of locked-folder assets.")
	dOffline      = desc("assets_offline", "Number of offline assets (file missing on disk).")
	dMotion       = desc("assets_motion", "Number of motion / live-photo assets.")
	dNotInAlbum   = desc("assets_not_in_album", "Number of assets not assigned to any album.")
	dEncoded      = desc("assets_encoded", "Number of transcoded video assets.")
	dTrashed      = desc("assets_trashed", "Number of trashed assets.")

	// cameras / EXIF
	dCameraMakes   = desc("camera_makes", "Number of distinct camera makes in the library.")
	dCameraModels  = desc("camera_models", "Number of distinct camera models in the library.")
	dLenses        = desc("lenses", "Number of distinct lens models in the library.")
	dAssetsByMake  = desc("assets_by_camera_make", "Assets per camera make.", "make")
	dAssetsByModel = desc("assets_by_camera_model", "Assets per camera model.", "model")
	dAssetsByLens  = desc("assets_by_lens", "Assets per lens model.", "lens")

	// geo
	dCities          = desc("places_cities", "Number of distinct cities with assets.")
	dStates          = desc("places_states", "Number of distinct states/provinces with assets.")
	dCountries       = desc("places_countries", "Number of distinct countries with assets.")
	dGeotagged       = desc("assets_geotagged", "Number of geotagged assets (from /map/markers).")
	dAssetsByCountry = desc("assets_by_country", "Geotagged assets per country. lat/lon = centroid of the country's own assets for a coords-mode geomap (no country-name mapping needed; works for every country).", "country", "lat", "lon")
	dAssetsByCity    = desc("assets_by_city", "Geotagged assets per city. lat/lon = centroid of the city's own assets for a coords-mode geomap.", "city", "country", "lat", "lon")

	// people
	dPeople          = desc("people", "Total number of people (including hidden).")
	dPeopleHidden    = desc("people_hidden", "Number of people marked hidden.")
	dPeopleNamed     = desc("people_named", "Number of named (identified) people.")
	dPeopleUnnamed   = desc("people_unnamed", "Number of unnamed people (detected but unlabelled).")
	dPeopleBirthdate = desc("people_with_birthdate", "Number of people with a birth date set.")
	dPersonAssets    = desc("person_assets", "Assets per person (top-N by asset count).", "person_id", "person")

	// users
	dUsers       = desc("users", "Number of users by status.", "status")
	dUsersByRole = desc("users_by_role", "Number of users by role.", "role")
	dUserPhotos  = desc("user_photos", "Per-user photo count.", "user_id", "user")
	dUserVideos  = desc("user_videos", "Per-user video count.", "user_id", "user")
	dUserUsage   = desc("user_usage_bytes", "Per-user total storage usage, in bytes.", "user_id", "user")
	dUserQuota   = desc("user_quota_bytes", "Per-user storage quota in bytes (omitted when unlimited).", "user_id", "user")
	dUserUnlim   = desc("user_quota_unlimited", "1 if the user has no storage quota (unlimited).", "user_id", "user")

	// albums / sharing
	dAlbums          = desc("albums", "Number of albums by sharing state.", "shared")
	dAlbumsOwned     = desc("albums_owned", "Number of albums owned by the API key user.")
	dAlbumsSharedSt  = desc("albums_shared", "Number of albums shared (from /albums/statistics).")
	dAlbumsNotShared = desc("albums_not_shared", "Number of private (not shared) albums.")
	dAlbumAssets     = desc("album_assets", "Total asset memberships across all albums (assets may repeat).")
	dAlbumsEmpty     = desc("albums_empty", "Number of albums containing no assets.")
	dAlbumsWithLink  = desc("albums_with_shared_link", "Number of albums exposing a public shared link.")
	dAlbumMax        = desc("album_assets_max", "Asset count of the largest album.")
	dAlbumAvg        = desc("album_assets_avg", "Average asset count per album.")
	dTopAlbum        = desc("album_top_assets", "Asset count of the top-N largest albums.", "album_id", "album")

	dSharedLinks      = desc("shared_links", "Number of shared links by type.", "type")
	dSharedLinksExp   = desc("shared_links_expired", "Number of expired shared links still present.")
	dSharedLinksNever = desc("shared_links_never_expire", "Number of shared links with no expiry.")
	dSharedLinksPwd   = desc("shared_links_password_protected", "Number of password-protected shared links.")
	dPartners         = desc("partners", "Number of partner shares by direction (incoming/outgoing).", "direction")

	// content extras
	dTags          = desc("tags", "Number of tags.")
	dTagsRoot      = desc("tags_root", "Number of top-level (root) tags.")
	dMemories      = desc("memories", "Number of memories.")
	dDupSets       = desc("duplicate_sets", "Number of duplicate clusters detected.")
	dDupAssets     = desc("duplicate_assets", "Number of assets involved in duplicate sets.")
	dStacks        = desc("stacks", "Number of asset stacks.")
	dStackedAssets = desc("stacked_assets", "Number of assets grouped into stacks.")
	dLibraries     = desc("libraries", "Number of configured external libraries.")
	dLibAssets     = desc("library_assets", "Assets per external library.", "library_id", "library")
	dAPIKeys       = desc("api_keys", "Number of API keys for the key owner.")
	dSessions      = desc("sessions", "Number of active sessions for the key owner.")
	dNotifUnread   = desc("notifications_unread", "Number of unread notifications.")
	dNotifByLevel  = desc("notifications_by_level", "Notifications by level.", "level")

	// jobs
	dJobQueue       = desc("job_queue", "Jobs in each queue by state.", "queue", "state")
	dJobQueuePaused = desc("job_queue_paused", "1 if a job queue is paused.", "queue")
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
		emit(ch, dExporterUp, 0)
		emit(ch, dServerUp, 0)
		return
	}
	emit(ch, dExporterUp, s.health.scrapeSuccess)
	emit(ch, dScrapeDuration, s.health.scrapeDurationSeconds)
	emit(ch, dLastSuccess, s.health.lastSuccessUnix)
	emit(ch, dKeyIsAdmin, s.health.keyIsAdmin)

	emitServer(ch, s)
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

func emitServer(ch chan<- prometheus.Metric, s *snapshot) {
	sv := &s.server
	emit(ch, dServerUp, s.health.serverUp)
	if a := sv.about; a.ok {
		emit(ch, dServerInfo, 1, a.version, a.sourceRef, a.sourceCommit, a.build,
			a.nodejs, a.exiftool, a.ffmpeg, a.imagemagick, a.libvips)
	}
	emit(ch, dServerLicensed, sv.licensed)
	if sv.latestVersion != "" {
		emit(ch, dLatestVersion, 1, sv.latestVersion)
		emit(ch, dUpdateAvailable, sv.updateAvail)
	}
	for f, v := range sv.features {
		emit(ch, dFeature, v, f)
	}
	emitOpt(ch, dConfigTrashDays, sv.trashDays)
	emitOpt(ch, dConfigDelDelay, sv.userDeleteDelayDays)
	emitOpt(ch, dConfigMinFaces, sv.minFaces)
	emitOpt(ch, dInitialized, sv.initialized)
	emitOpt(ch, dOnboarded, sv.onboarded)
	emitOpt(ch, dStorageSize, sv.storageSizeBytes)
	emitOpt(ch, dStorageUsed, sv.storageUsedBytes)
	emitOpt(ch, dStorageAvail, sv.storageAvailableBytes)
}

func emitAssets(ch chan<- prometheus.Metric, s *snapshot) {
	a := &s.assets
	for t, v := range a.byType {
		emit(ch, dAssets, v, t)
	}
	for t, v := range a.storageByType {
		emit(ch, dAssetStorage, v, t)
	}
	emitOpt(ch, dServerUsage, a.serverUsageBytes)
	for y, v := range a.byYear {
		emit(ch, dAssetsByYear, v, y)
	}
	for r, v := range a.byRating {
		emit(ch, dAssetsByRate, v, r)
	}
	emitOpt(ch, dFavorite, a.favorite)
	emitOpt(ch, dArchived, a.archived)
	emitOpt(ch, dHidden, a.hidden)
	emitOpt(ch, dLocked, a.locked)
	emitOpt(ch, dOffline, a.offline)
	emitOpt(ch, dMotion, a.motion)
	emitOpt(ch, dNotInAlbum, a.notInAlbum)
	emitOpt(ch, dEncoded, a.encoded)
	emitOpt(ch, dTrashed, a.trashed)
}

func emitCameras(ch chan<- prometheus.Metric, s *snapshot) {
	c := &s.cameras
	emitOpt(ch, dCameraMakes, c.makes)
	emitOpt(ch, dCameraModels, c.models)
	emitOpt(ch, dLenses, c.lenses)
	for k, v := range c.byMake {
		emit(ch, dAssetsByMake, v, k)
	}
	for k, v := range c.byModel {
		emit(ch, dAssetsByModel, v, k)
	}
	for k, v := range c.byLens {
		emit(ch, dAssetsByLens, v, k)
	}
}

func emitGeo(ch chan<- prometheus.Metric, s *snapshot) {
	g := &s.geo
	emitOpt(ch, dCities, g.cities)
	emitOpt(ch, dStates, g.states)
	emitOpt(ch, dCountries, g.countries)
	emitOpt(ch, dGeotagged, g.geotagged)
	for k, v := range g.byCountry {
		c := g.countryCentroids[k]
		emit(ch, dAssetsByCountry, v, k, c.lat, c.lon)
	}
	for k, v := range g.byCity {
		c := g.cityCentroids[k]
		emit(ch, dAssetsByCity, v, k.city, k.country, c.lat, c.lon)
	}
}

func emitPeople(ch chan<- prometheus.Metric, s *snapshot) {
	p := &s.people
	if p.ok {
		emit(ch, dPeople, p.total)
		emit(ch, dPeopleHidden, p.hidden)
		emit(ch, dPeopleNamed, p.named)
		emit(ch, dPeopleUnnamed, p.unnamed)
		emit(ch, dPeopleBirthdate, p.withBirthdate)
	}
	for _, pe := range p.assets {
		emit(ch, dPersonAssets, pe.value, pe.id, pe.name)
	}
}

func emitUsers(ch chan<- prometheus.Metric, s *snapshot) {
	u := &s.users
	if u.ok {
		for st, v := range u.byStatus {
			emit(ch, dUsers, v, st)
		}
		for r, v := range u.byRole {
			emit(ch, dUsersByRole, v, r)
		}
	}
	for _, us := range u.perUser {
		emit(ch, dUserPhotos, us.photos, us.id, us.name)
		emit(ch, dUserVideos, us.videos, us.id, us.name)
		emit(ch, dUserUsage, us.usageBytes, us.id, us.name)
		if us.quotaUnlimited {
			emit(ch, dUserUnlim, 1, us.id, us.name)
		} else {
			emit(ch, dUserUnlim, 0, us.id, us.name)
			emit(ch, dUserQuota, us.quotaBytes, us.id, us.name)
		}
	}
}

func emitAlbums(ch chan<- prometheus.Metric, s *snapshot) {
	a := &s.albums
	if a.statsOK {
		emit(ch, dAlbumsOwned, a.owned)
		emit(ch, dAlbumsSharedSt, a.shared)
		emit(ch, dAlbumsNotShared, a.notShared)
	}
	if a.ok {
		emit(ch, dAlbums, a.sharedCount, "true")
		emit(ch, dAlbums, a.privateCount, "false")
		emit(ch, dAlbumAssets, a.assetsTotal)
		emit(ch, dAlbumsEmpty, a.empty)
		emit(ch, dAlbumsWithLink, a.withSharedLink)
		emit(ch, dAlbumMax, a.assetsMax)
		emit(ch, dAlbumAvg, a.assetsAvg)
	}
	for _, al := range a.top {
		emit(ch, dTopAlbum, al.value, al.id, al.name)
	}
	if sl := &a.sharedLinks; sl.ok {
		for t, v := range sl.byType {
			emit(ch, dSharedLinks, v, t)
		}
		emit(ch, dSharedLinksExp, sl.expired)
		emit(ch, dSharedLinksNever, sl.neverExpire)
		emit(ch, dSharedLinksPwd, sl.passwordProtected)
	}
	for dir, v := range a.partners {
		emit(ch, dPartners, v, dir)
	}
}

func emitContent(ch chan<- prometheus.Metric, s *snapshot) {
	c := &s.content
	if c.tagsOK {
		emit(ch, dTags, c.tags)
		emit(ch, dTagsRoot, c.tagsRoot)
	}
	emitOpt(ch, dMemories, c.memories)
	if c.duplicatesOK {
		emit(ch, dDupSets, c.duplicateSets)
		emit(ch, dDupAssets, c.duplicateAssets)
	}
	if c.stacksOK {
		emit(ch, dStacks, c.stacks)
		emit(ch, dStackedAssets, c.stackedAssets)
	}
	emitOpt(ch, dLibraries, c.libraries)
	for _, l := range c.perLibrary {
		emit(ch, dLibAssets, l.value, l.id, l.name)
	}
	emitOpt(ch, dAPIKeys, c.apiKeys)
	emitOpt(ch, dSessions, c.sessions)
	if c.notifOK {
		emit(ch, dNotifUnread, c.notifUnread)
		for lvl, v := range c.notifByLevel {
			emit(ch, dNotifByLevel, v, lvl)
		}
	}
}

func emitJobs(ch chan<- prometheus.Metric, s *snapshot) {
	for _, jq := range s.jobs.queues {
		emit(ch, dJobQueue, jq.count, jq.queue, jq.state)
	}
	for q, v := range s.jobs.paused {
		emit(ch, dJobQueuePaused, v, q)
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
