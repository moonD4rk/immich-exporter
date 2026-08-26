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
	s.health = healthSnap{serverUp: 1, scrapeSuccess: 1, scrapeDurationSeconds: 0.42, lastSuccessUnix: 1.7e9, keyIsAdmin: 1}
	s.server.about = aboutInfo{version: "v1.120.0", sourceRef: "main", ffmpeg: "6.0", ok: true}
	s.server.licensed = 1
	s.server.latestVersion, s.server.updateAvail = "v1.121.0", 1
	s.server.features["smart_search"] = 1
	s.server.features["facial_recognition"] = 0
	s.server.trashDays = set(30)
	s.server.userDeleteDelayDays = set(7)
	s.server.minFaces = set(3)
	s.server.initialized = set(1)
	s.server.onboarded = set(1)
	s.server.storageSizeBytes, s.server.storageUsedBytes, s.server.storageAvailableBytes = set(1e12), set(4e11), set(6e11)
	s.assets.byType["IMAGE"], s.assets.byType["VIDEO"] = 1000, 50
	s.assets.storageByType["IMAGE"], s.assets.storageByType["VIDEO"] = 9e9, 3e9
	s.assets.serverUsageBytes = set(12e9)
	s.assets.byYear["2024"], s.assets.byYear["2023"] = 600, 450
	s.assets.byRating["5"], s.assets.byRating["unrated"] = 10, 990
	s.assets.favorite = set(12)
	s.assets.archived = set(5)
	s.assets.hidden = set(1)
	s.assets.locked = set(0)
	s.assets.offline = set(2)
	s.assets.motion = set(30)
	s.assets.notInAlbum = set(400)
	s.assets.encoded = set(40)
	s.assets.trashed = set(3)
	s.cameras.makes, s.cameras.models, s.cameras.lenses = set(4), set(9), set(6)
	s.cameras.byMake = map[string]float64{"Apple": 800, "Sony": 200}
	s.cameras.byModel = map[string]float64{"iPhone 15 Pro": 700, "other": 350}
	s.cameras.byLens = map[string]float64{"24mm": 100}
	s.geo.cities, s.geo.states, s.geo.countries, s.geo.geotagged = set(12), set(8), set(3), set(720)
	s.geo.byCountry = map[string]float64{"China": 500, "Japan": 200, "unknown": 20}
	s.geo.countryCentroids = map[string]latlon{"China": {"31.2", "121.5"}, "Japan": {"35.7", "139.7"}}
	s.geo.byCity = map[cityKey]float64{
		{city: "Shanghai", country: "China"}: 450,
		{city: "unknown", country: "China"}:  50,
	}
	s.geo.cityCentroids = map[cityKey]latlon{{city: "Shanghai", country: "China"}: {"31.23", "121.47"}}
	s.people = peopleSnap{
		total: 40, hidden: 2, named: 25, unnamed: 15, withBirthdate: 5, ok: true,
		assets: []labeledVal{{"id1", "Alice", 300}, {"id2", "(unnamed)", 120}},
	}
	s.users.ok = true
	s.users.byStatus["active"], s.users.byStatus["deleted"] = 3, 1
	s.users.byRole["admin"], s.users.byRole["user"] = 1, 3
	s.users.perUser = []userStat{
		{id: "u1", name: "admin", photos: 1000, videos: 50, usageBytes: 12e9, quotaUnlimited: true},
		{id: "u2", name: "bob", photos: 10, videos: 0, usageBytes: 1e8, quotaBytes: 5e9},
	}
	s.albums.owned, s.albums.shared, s.albums.notShared, s.albums.statsOK = 8, 3, 5, true
	s.albums.sharedCount, s.albums.privateCount = 3, 5
	s.albums.assetsTotal, s.albums.empty, s.albums.withSharedLink = 1200, 1, 2
	s.albums.assetsMax, s.albums.assetsAvg, s.albums.ok = 500, 150, true
	s.albums.top = []labeledVal{{"a1", "Trips", 500}, {"a2", "Family", 300}}
	s.albums.sharedLinks.byType["ALBUM"], s.albums.sharedLinks.byType["INDIVIDUAL"] = 2, 1
	s.albums.sharedLinks.expired, s.albums.sharedLinks.neverExpire = 1, 1
	s.albums.sharedLinks.passwordProtected, s.albums.sharedLinks.ok = 1, true
	s.albums.partners["incoming"], s.albums.partners["outgoing"] = 1, 2
	s.content.tags, s.content.tagsRoot, s.content.tagsOK = 20, 5, true
	s.content.memories = set(7)
	s.content.duplicateSets, s.content.duplicateAssets, s.content.duplicatesOK = 4, 9, true
	s.content.stacks, s.content.stackedAssets, s.content.stacksOK = 3, 8, true
	s.content.libraries = set(2)
	s.content.perLibrary = []labeledVal{{"l1", "Photos", 5000}}
	s.content.apiKeys = set(3)
	s.content.sessions = set(2)
	s.content.notifUnread, s.content.notifOK = 4, true
	s.content.notifByLevel["error"], s.content.notifByLevel["info"] = 1, 3
	s.jobs.queues = []jobQueueStat{{"smartSearch", "waiting", 12}, {"smartSearch", "active", 1}}
	s.jobs.paused["smartSearch"] = 0
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
		`immich_assets_by_city{city="Shanghai",country="China",lat="31.23",lon="121.47"} 450`,
		`immich_assets_by_city{city="unknown",country="China",lat="",lon=""} 50`,
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
