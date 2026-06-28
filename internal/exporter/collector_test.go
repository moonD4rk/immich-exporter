package exporter

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// fullSnapshot exercises every metric path with at least one labeled value so
// Gather() surfaces any const-metric label-cardinality mismatch (which would
// otherwise panic only at scrape time) or duplicate series.
func fullSnapshot() *snapshot {
	s := newSnapshot()
	s.serverUp, s.scrapeSuccess, s.scrapeDurationSeconds, s.lastSuccessUnix, s.keyIsAdmin = 1, 1, 0.42, 1.7e9, 1
	s.serverInfo = map[string]string{"version": "v1.120.0", "source_ref": "main", "ffmpeg": "6.0"}
	s.serverLicensed = 1
	s.latestVersion, s.updateAvail = "v1.121.0", 1
	s.features["smart_search"] = 1
	s.features["facial_recognition"] = 0
	s.configTrashDays, s.hasConfigTrashDays = 30, true
	s.configUserDeleteDelayDays, s.hasConfigDeleteDelay = 7, true
	s.configMinFaces, s.hasConfigMinFaces = 3, true
	s.serverInitialized, s.hasInitialized = 1, true
	s.serverOnboarded, s.hasOnboarded = 1, true
	s.storageSizeBytes, s.storageUsedBytes, s.storageAvailableBytes, s.hasStorage = 1e12, 4e11, 6e11, true
	s.assets["IMAGE"], s.assets["VIDEO"] = 1000, 50
	s.assetStorage["IMAGE"], s.assetStorage["VIDEO"] = 9e9, 3e9
	s.serverUsageBytes, s.hasServerUsage = 12e9, true
	s.assetsByYear["2024"], s.assetsByYear["2023"] = 600, 450
	s.assetsByRating["5"], s.assetsByRating["unrated"] = 10, 990
	s.assetsFavorite, s.hasFavorite = 12, true
	s.assetsArchived, s.hasArchived = 5, true
	s.assetsHidden, s.hasHidden = 1, true
	s.assetsLocked, s.hasLocked = 0, true
	s.assetsOffline, s.hasOffline = 2, true
	s.assetsMotion, s.hasMotion = 30, true
	s.assetsNotInAlbum, s.hasNotInAlbum = 400, true
	s.assetsEncoded, s.hasEncoded = 40, true
	s.assetsTrashed, s.hasTrashed = 3, true
	s.cameraMakes, s.hasCameraMakes = 4, true
	s.cameraModels, s.hasCameraModels = 9, true
	s.lenses, s.hasLenses = 6, true
	s.assetsByMake = map[string]float64{"Apple": 800, "Sony": 200}
	s.assetsByModel = map[string]float64{"iPhone 15 Pro": 700, "other": 350}
	s.assetsByLens = map[string]float64{"24mm": 100}
	s.cities, s.hasCities = 12, true
	s.states, s.hasStates = 8, true
	s.countries, s.hasCountries = 3, true
	s.geotagged, s.hasGeotagged = 720, true
	s.assetsByCountry = map[string]float64{"China": 500, "Japan": 200, "unknown": 20}
	s.geoCentroids = map[string][2]string{"China": {"31.2", "121.5"}, "Japan": {"35.7", "139.7"}}
	s.people, s.peopleHidden, s.peopleNamed, s.peopleUnnamed, s.peopleWithBirthdate, s.hasPeople = 40, 2, 25, 15, 5, true
	s.personAssets = []labeledVal{{"id1", "Alice", 300}, {"id2", "(unnamed)", 120}}
	s.hasUsers = true
	s.usersByStatus["active"], s.usersByStatus["deleted"] = 3, 1
	s.usersByRole["admin"], s.usersByRole["user"] = 1, 3
	s.perUser = []userStat{
		{id: "u1", name: "admin", photos: 1000, videos: 50, usageBytes: 12e9, quotaUnlimited: true},
		{id: "u2", name: "bob", photos: 10, videos: 0, usageBytes: 1e8, quotaBytes: 5e9},
	}
	s.albumsOwned, s.albumsShared, s.albumsNotShared, s.hasAlbumStats = 8, 3, 5, true
	s.albumsSharedCount, s.albumsPrivateCount = 3, 5
	s.albumAssets, s.albumsEmpty, s.albumsWithSharedLink, s.albumAssetsMax, s.albumAssetsAvg, s.hasAlbums = 1200, 1, 2, 500, 150, true
	s.topAlbums = []labeledVal{{"a1", "Trips", 500}, {"a2", "Family", 300}}
	s.sharedLinks["ALBUM"], s.sharedLinks["INDIVIDUAL"] = 2, 1
	s.sharedLinksExpired, s.sharedLinksNeverExpire, s.sharedLinksPasswordProtected, s.hasSharedLinks = 1, 1, 1, true
	s.partners["incoming"], s.partners["outgoing"] = 1, 2
	s.tags, s.tagsRoot, s.hasTags = 20, 5, true
	s.memories, s.hasMemories = 7, true
	s.duplicateSets, s.duplicateAssets, s.hasDuplicates = 4, 9, true
	s.stacks, s.stackedAssets, s.hasStacks = 3, 8, true
	s.libraries, s.hasLibraries = 2, true
	s.perLibrary = []labeledVal{{"l1", "Photos", 5000}}
	s.apiKeys, s.hasAPIKeys = 3, true
	s.sessions, s.hasSessions = 2, true
	s.notificationsUnread, s.hasNotifications = 4, true
	s.notificationsByLevel["error"], s.notificationsByLevel["info"] = 1, 3
	s.jobQueues = []jobQueueStat{{"smartSearch", "waiting", 12}, {"smartSearch", "active", 1}}
	s.jobQueuePaused["smartSearch"] = 0
	return s
}

func TestCollectNoSnapshot(t *testing.T) {
	reg := prometheus.NewRegistry()
	var errs atomic.Int64
	var snap atomic.Pointer[snapshot]
	reg.MustRegister(&collector{snap: &snap, scrapeErrors: &errs, version: "test", goVersion: "go1"})
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather with nil snapshot: %v", err)
	}
}

func TestCollectFullSnapshot(t *testing.T) {
	reg := prometheus.NewRegistry()
	var errs atomic.Int64
	errs.Store(5)
	var snap atomic.Pointer[snapshot]
	snap.Store(fullSnapshot())
	reg.MustRegister(&collector{snap: &snap, scrapeErrors: &errs, version: "test", goVersion: "go1"})

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather full snapshot: %v", err)
	}
	if len(mfs) < 40 {
		t.Fatalf("expected many metric families, got %d", len(mfs))
	}

	// Spot-check a couple of representative values via the text exposition.
	want := []string{
		`immich_assets{type="IMAGE"} 1000`,
		`immich_assets{type="VIDEO"} 50`,
		`immich_exporter_scrape_errors_total 5`,
		`immich_job_queue{queue="smartSearch",state="waiting"} 12`,
		`immich_user_quota_unlimited{user="admin",user_id="u1"} 1`,
		`immich_assets_by_country{country="China",lat="31.2",lon="121.5"} 500`,
	}
	out := renderText(t, reg)
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("metrics output missing %q", w)
		}
	}
}

func renderText(t *testing.T, g prometheus.Gatherer) string {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	enc := expfmt.NewEncoder(&b, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}
