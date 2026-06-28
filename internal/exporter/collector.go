package exporter

import (
	"context"
	"runtime"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/moonD4rk/immich-exporter/internal/immich"
)

const ns = "immich"

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

// jobQueueStat is one (queue,state) -> count datapoint plus the paused flag.
type jobQueueStat struct {
	queue, state string
	count        float64
}

// snapshot is the complete set of values from one poll cycle. The collector
// emits it verbatim on every Prometheus scrape, so a label combination that
// disappears upstream simply stops being emitted (no stale series).
type snapshot struct {
	// exporter self-health
	serverUp              float64
	scrapeSuccess         float64
	scrapeDurationSeconds float64
	lastSuccessUnix       float64
	keyIsAdmin            float64

	// server / system
	serverInfo     map[string]string // version, source_ref, source_commit, build, nodejs, exiftool, ffmpeg, imagemagick, libvips
	serverLicensed float64
	latestVersion  string
	updateAvail    float64
	features       map[string]float64 // feature -> 0/1
	configTrashDays,
	configUserDeleteDelayDays,
	configMinFaces,
	serverInitialized,
	serverOnboarded float64
	hasConfigTrashDays, hasConfigDeleteDelay, hasConfigMinFaces, hasInitialized, hasOnboarded bool

	// storage (volume backing UPLOAD_LOCATION)
	storageSizeBytes, storageUsedBytes, storageAvailableBytes float64
	hasStorage                                                bool

	// assets — instance-wide via /server/statistics (admin) or owner-scoped fallback
	assets           map[string]float64 // type -> count
	assetStorage     map[string]float64 // type -> bytes
	serverUsageBytes float64
	hasServerUsage   bool

	// assets by year
	assetsByYear map[string]float64

	// asset state breakdowns (owner-scoped)
	assetsFavorite, assetsArchived, assetsHidden, assetsLocked,
	assetsOffline, assetsMotion, assetsNotInAlbum, assetsEncoded, assetsTrashed float64
	hasFavorite, hasArchived, hasHidden, hasLocked,
	hasOffline, hasMotion, hasNotInAlbum, hasEncoded, hasTrashed bool
	assetsByRating map[string]float64 // "1".."5","unrated"

	// cameras / EXIF
	cameraMakes, cameraModels, lenses          float64
	hasCameraMakes, hasCameraModels, hasLenses bool
	assetsByMake                               map[string]float64
	assetsByModel                              map[string]float64
	assetsByLens                               map[string]float64

	// geo
	cities, states, countries          float64
	hasCities, hasStates, hasCountries bool
	geotagged                          float64
	hasGeotagged                       bool
	assetsByCountry                    map[string]float64   // country -> count
	geoCentroids                       map[string][2]string // country -> [lat, lon] of its assets

	// people
	people, peopleHidden, peopleNamed, peopleUnnamed, peopleWithBirthdate float64
	hasPeople                                                             bool
	personAssets                                                          []labeledVal

	// users
	usersByStatus map[string]float64
	usersByRole   map[string]float64
	hasUsers      bool
	perUser       []userStat

	// albums / sharing
	albumsOwned, albumsShared, albumsNotShared float64
	hasAlbumStats                              bool
	albumsSharedCount, albumsPrivateCount,
	albumAssets, albumsEmpty, albumsWithSharedLink,
	albumAssetsMax, albumAssetsAvg float64
	hasAlbums bool
	topAlbums []labeledVal

	sharedLinks                                                              map[string]float64 // type -> count
	sharedLinksExpired, sharedLinksNeverExpire, sharedLinksPasswordProtected float64
	hasSharedLinks                                                           bool
	partners                                                                 map[string]float64 // direction -> count

	// content extras
	tags, tagsRoot                 float64
	hasTags                        bool
	memories                       float64
	hasMemories                    bool
	duplicateSets, duplicateAssets float64
	hasDuplicates                  bool
	stacks, stackedAssets          float64
	hasStacks                      bool
	libraries                      float64
	hasLibraries                   bool
	perLibrary                     []labeledVal
	apiKeys                        float64
	hasAPIKeys                     bool
	sessions                       float64
	hasSessions                    bool
	notificationsUnread            float64
	hasNotifications               bool
	notificationsByLevel           map[string]float64

	// jobs
	jobQueues      []jobQueueStat
	jobQueuePaused map[string]float64
}

func newSnapshot() *snapshot {
	return &snapshot{
		serverInfo:           map[string]string{},
		features:             map[string]float64{},
		assets:               map[string]float64{},
		assetStorage:         map[string]float64{},
		assetsByYear:         map[string]float64{},
		assetsByRating:       map[string]float64{},
		assetsByMake:         map[string]float64{},
		assetsByModel:        map[string]float64{},
		assetsByLens:         map[string]float64{},
		assetsByCountry:      map[string]float64{},
		geoCentroids:         map[string][2]string{},
		usersByStatus:        map[string]float64{},
		usersByRole:          map[string]float64{},
		sharedLinks:          map[string]float64{},
		partners:             map[string]float64{},
		notificationsByLevel: map[string]float64{},
		jobQueuePaused:       map[string]float64{},
	}
}

func desc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(ns, "", name), help, labels, nil)
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

var allDescs = []*prometheus.Desc{
	dExporterUp, dScrapeDuration, dLastSuccess, dScrapeErrors, dExporterBuildInfo, dKeyIsAdmin,
	dServerUp, dServerInfo, dServerLicensed, dLatestVersion, dUpdateAvailable, dFeature,
	dConfigTrashDays, dConfigDelDelay, dConfigMinFaces, dInitialized, dOnboarded,
	dStorageSize, dStorageUsed, dStorageAvail,
	dAssets, dAssetStorage, dServerUsage, dAssetsByYear, dAssetsByRate,
	dFavorite, dArchived, dHidden, dLocked, dOffline, dMotion, dNotInAlbum, dEncoded, dTrashed,
	dCameraMakes, dCameraModels, dLenses, dAssetsByMake, dAssetsByModel, dAssetsByLens,
	dCities, dStates, dCountries, dGeotagged, dAssetsByCountry,
	dPeople, dPeopleHidden, dPeopleNamed, dPeopleUnnamed, dPeopleBirthdate, dPersonAssets,
	dUsers, dUsersByRole, dUserPhotos, dUserVideos, dUserUsage, dUserQuota, dUserUnlim,
	dAlbums, dAlbumsOwned, dAlbumsSharedSt, dAlbumsNotShared, dAlbumAssets, dAlbumsEmpty,
	dAlbumsWithLink, dAlbumMax, dAlbumAvg, dTopAlbum,
	dSharedLinks, dSharedLinksExp, dSharedLinksNever, dSharedLinksPwd, dPartners,
	dTags, dTagsRoot, dMemories, dDupSets, dDupAssets, dStacks, dStackedAssets,
	dLibraries, dLibAssets, dAPIKeys, dSessions, dNotifUnread, dNotifByLevel,
	dJobQueue, dJobQueuePaused,
}

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
	emit(ch, dExporterUp, s.scrapeSuccess)
	emit(ch, dScrapeDuration, s.scrapeDurationSeconds)
	emit(ch, dLastSuccess, s.lastSuccessUnix)
	emit(ch, dKeyIsAdmin, s.keyIsAdmin)

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

func emit(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, lv ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, lv...)
}

func emitOpt(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, ok bool) {
	if ok {
		emit(ch, d, v)
	}
}

func emitServer(ch chan<- prometheus.Metric, s *snapshot) {
	emit(ch, dServerUp, s.serverUp)
	if len(s.serverInfo) > 0 {
		si := s.serverInfo
		emit(ch, dServerInfo, 1, si["version"], si["source_ref"], si["source_commit"], si["build"],
			si["nodejs"], si["exiftool"], si["ffmpeg"], si["imagemagick"], si["libvips"])
	}
	emit(ch, dServerLicensed, s.serverLicensed)
	if s.latestVersion != "" {
		emit(ch, dLatestVersion, 1, s.latestVersion)
		emit(ch, dUpdateAvailable, s.updateAvail)
	}
	for f, v := range s.features {
		emit(ch, dFeature, v, f)
	}
	emitOpt(ch, dConfigTrashDays, s.configTrashDays, s.hasConfigTrashDays)
	emitOpt(ch, dConfigDelDelay, s.configUserDeleteDelayDays, s.hasConfigDeleteDelay)
	emitOpt(ch, dConfigMinFaces, s.configMinFaces, s.hasConfigMinFaces)
	emitOpt(ch, dInitialized, s.serverInitialized, s.hasInitialized)
	emitOpt(ch, dOnboarded, s.serverOnboarded, s.hasOnboarded)
	if s.hasStorage {
		emit(ch, dStorageSize, s.storageSizeBytes)
		emit(ch, dStorageUsed, s.storageUsedBytes)
		emit(ch, dStorageAvail, s.storageAvailableBytes)
	}
}

func emitAssets(ch chan<- prometheus.Metric, s *snapshot) {
	for t, v := range s.assets {
		emit(ch, dAssets, v, t)
	}
	for t, v := range s.assetStorage {
		emit(ch, dAssetStorage, v, t)
	}
	emitOpt(ch, dServerUsage, s.serverUsageBytes, s.hasServerUsage)
	for y, v := range s.assetsByYear {
		emit(ch, dAssetsByYear, v, y)
	}
	for r, v := range s.assetsByRating {
		emit(ch, dAssetsByRate, v, r)
	}
	emitOpt(ch, dFavorite, s.assetsFavorite, s.hasFavorite)
	emitOpt(ch, dArchived, s.assetsArchived, s.hasArchived)
	emitOpt(ch, dHidden, s.assetsHidden, s.hasHidden)
	emitOpt(ch, dLocked, s.assetsLocked, s.hasLocked)
	emitOpt(ch, dOffline, s.assetsOffline, s.hasOffline)
	emitOpt(ch, dMotion, s.assetsMotion, s.hasMotion)
	emitOpt(ch, dNotInAlbum, s.assetsNotInAlbum, s.hasNotInAlbum)
	emitOpt(ch, dEncoded, s.assetsEncoded, s.hasEncoded)
	emitOpt(ch, dTrashed, s.assetsTrashed, s.hasTrashed)
}

func emitCameras(ch chan<- prometheus.Metric, s *snapshot) {
	emitOpt(ch, dCameraMakes, s.cameraMakes, s.hasCameraMakes)
	emitOpt(ch, dCameraModels, s.cameraModels, s.hasCameraModels)
	emitOpt(ch, dLenses, s.lenses, s.hasLenses)
	for k, v := range s.assetsByMake {
		emit(ch, dAssetsByMake, v, k)
	}
	for k, v := range s.assetsByModel {
		emit(ch, dAssetsByModel, v, k)
	}
	for k, v := range s.assetsByLens {
		emit(ch, dAssetsByLens, v, k)
	}
}

func emitGeo(ch chan<- prometheus.Metric, s *snapshot) {
	emitOpt(ch, dCities, s.cities, s.hasCities)
	emitOpt(ch, dStates, s.states, s.hasStates)
	emitOpt(ch, dCountries, s.countries, s.hasCountries)
	emitOpt(ch, dGeotagged, s.geotagged, s.hasGeotagged)
	for k, v := range s.assetsByCountry {
		lat, lon := "", ""
		if cc, ok := s.geoCentroids[k]; ok {
			lat, lon = cc[0], cc[1]
		}
		emit(ch, dAssetsByCountry, v, k, lat, lon)
	}
}

func emitPeople(ch chan<- prometheus.Metric, s *snapshot) {
	if s.hasPeople {
		emit(ch, dPeople, s.people)
		emit(ch, dPeopleHidden, s.peopleHidden)
		emit(ch, dPeopleNamed, s.peopleNamed)
		emit(ch, dPeopleUnnamed, s.peopleUnnamed)
		emit(ch, dPeopleBirthdate, s.peopleWithBirthdate)
	}
	for _, p := range s.personAssets {
		emit(ch, dPersonAssets, p.value, p.id, p.name)
	}
}

func emitUsers(ch chan<- prometheus.Metric, s *snapshot) {
	if s.hasUsers {
		for st, v := range s.usersByStatus {
			emit(ch, dUsers, v, st)
		}
		for r, v := range s.usersByRole {
			emit(ch, dUsersByRole, v, r)
		}
	}
	for _, u := range s.perUser {
		emit(ch, dUserPhotos, u.photos, u.id, u.name)
		emit(ch, dUserVideos, u.videos, u.id, u.name)
		emit(ch, dUserUsage, u.usageBytes, u.id, u.name)
		if u.quotaUnlimited {
			emit(ch, dUserUnlim, 1, u.id, u.name)
		} else {
			emit(ch, dUserUnlim, 0, u.id, u.name)
			emit(ch, dUserQuota, u.quotaBytes, u.id, u.name)
		}
	}
}

func emitAlbums(ch chan<- prometheus.Metric, s *snapshot) {
	if s.hasAlbumStats {
		emit(ch, dAlbumsOwned, s.albumsOwned)
		emit(ch, dAlbumsSharedSt, s.albumsShared)
		emit(ch, dAlbumsNotShared, s.albumsNotShared)
	}
	if s.hasAlbums {
		emit(ch, dAlbums, s.albumsSharedCount, "true")
		emit(ch, dAlbums, s.albumsPrivateCount, "false")
		emit(ch, dAlbumAssets, s.albumAssets)
		emit(ch, dAlbumsEmpty, s.albumsEmpty)
		emit(ch, dAlbumsWithLink, s.albumsWithSharedLink)
		emit(ch, dAlbumMax, s.albumAssetsMax)
		emit(ch, dAlbumAvg, s.albumAssetsAvg)
	}
	for _, a := range s.topAlbums {
		emit(ch, dTopAlbum, a.value, a.id, a.name)
	}
	if s.hasSharedLinks {
		for t, v := range s.sharedLinks {
			emit(ch, dSharedLinks, v, t)
		}
		emit(ch, dSharedLinksExp, s.sharedLinksExpired)
		emit(ch, dSharedLinksNever, s.sharedLinksNeverExpire)
		emit(ch, dSharedLinksPwd, s.sharedLinksPasswordProtected)
	}
	for dir, v := range s.partners {
		emit(ch, dPartners, v, dir)
	}
}

func emitContent(ch chan<- prometheus.Metric, s *snapshot) {
	if s.hasTags {
		emit(ch, dTags, s.tags)
		emit(ch, dTagsRoot, s.tagsRoot)
	}
	emitOpt(ch, dMemories, s.memories, s.hasMemories)
	if s.hasDuplicates {
		emit(ch, dDupSets, s.duplicateSets)
		emit(ch, dDupAssets, s.duplicateAssets)
	}
	if s.hasStacks {
		emit(ch, dStacks, s.stacks)
		emit(ch, dStackedAssets, s.stackedAssets)
	}
	emitOpt(ch, dLibraries, s.libraries, s.hasLibraries)
	for _, l := range s.perLibrary {
		emit(ch, dLibAssets, l.value, l.id, l.name)
	}
	emitOpt(ch, dAPIKeys, s.apiKeys, s.hasAPIKeys)
	emitOpt(ch, dSessions, s.sessions, s.hasSessions)
	if s.hasNotifications {
		emit(ch, dNotifUnread, s.notificationsUnread)
		for lvl, v := range s.notificationsByLevel {
			emit(ch, dNotifByLevel, v, lvl)
		}
	}
}

func emitJobs(ch chan<- prometheus.Metric, s *snapshot) {
	for _, jq := range s.jobQueues {
		emit(ch, dJobQueue, jq.count, jq.queue, jq.state)
	}
	for q, v := range s.jobQueuePaused {
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
