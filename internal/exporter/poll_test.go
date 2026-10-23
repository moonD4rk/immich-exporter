package exporter

import (
	"context"
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/moond4rk/immich-exporter/internal/immich"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, v)
}

// searchStatsTotal returns a deterministic count for a StatisticsSearchDto body
// so per-filter breakdowns can be asserted.
func searchStatsTotal(m map[string]any) float64 {
	switch {
	case m["isFavorite"] == true:
		return 12
	case m["visibility"] == "archive":
		return 5
	case m["visibility"] == "hidden":
		return 1
	case m["isOffline"] == true:
		return 2
	case m["isMotion"] == true:
		return 30
	case m["isNotInAlbum"] == true:
		return 400
	case m["isEncoded"] == true:
		return 40
	case m["type"] == "IMAGE":
		return 1000
	case m["type"] == "VIDEO":
		return 50
	case m["make"] == "Apple":
		return 800
	case m["make"] == "Sony":
		return 200
	case m["model"] != nil:
		return 700
	case m["lensModel"] != nil:
		return 100
	}
	if v, ok := m["rating"]; ok {
		if v == nil {
			return 990
		}
		f, _ := v.(float64)
		return f
	}
	if f, ok := m["filter"].(map[string]any); ok {
		return filterTotal(f)
	}
	return 0
}

// filterTotal answers StatisticsSearchDto.filter bodies scoped to visible
// assets: 1050 of them, 6 without a recorded file size. An unscoped filter
// gets a total no assertion expects, as a live server would add the hidden
// video halves of live photos.
func filterTotal(f map[string]any) float64 {
	v, _ := f["visibility"].(map[string]any)
	if in, _ := v["in"].([]any); !slices.Equal(in, []any{"timeline", "archive"}) {
		return -1
	}
	f = maps.Clone(f)
	delete(f, "visibility")
	if len(f) == 0 {
		return 1050
	}
	if f["hasPeople"] != nil {
		return 600
	}
	if c, ok := f["make"].(map[string]any); ok && c["ne"] == nil {
		return 900
	}
	size, _ := f["fileSizeInBytes"].(map[string]any)
	if gte, ok := size["gte"].(float64); ok && gte == 1<<30 {
		return 4
	}
	below, _ := size["lt"].(float64)
	return map[float64]float64{1 << 20: 100, 5 << 20: 700, 20 << 20: 950, 100 << 20: 1000, 1 << 30: 1040}[below]
}

// mockReleaseCheck is when the mock server last checked for releases; the
// golden test pins the value.
var mockReleaseCheck = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)

func immichMock(t *testing.T, isAdmin bool) *httptest.Server {
	t.Helper()
	return serve(t, immichMux(isAdmin))
}

func serve(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// refuse answers the given paths with the status, like a key scoped without their permissions.
func refuse(h http.Handler, status int, paths ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(paths, r.URL.Path) {
			http.Error(w, `{"message":"refused"}`, status)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func immichMux(isAdmin bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /server/ping", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"res": "pong"})
	})
	mux.HandleFunc("GET /admin/maintenance/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"active": false, "action": "end"})
	})
	mux.HandleFunc("GET /users/me", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"isAdmin": isAdmin})
	})
	mux.HandleFunc("GET /server/about", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"version": "v3.3.4", "ffmpeg": "6.0", "licensed": true})
	})
	mux.HandleFunc("GET /server/version-check", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"releaseVersion": "v3.3.5", "checkedAt": mockReleaseCheck})
	})
	mux.HandleFunc("GET /server/version-history", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"id": "h2", "version": "3.3.4", "createdAt": "2026-10-20T10:30:00.000Z"},
			{"id": "h1", "version": "3.3.3", "createdAt": "2026-10-18T06:33:19.829Z"},
		})
	})
	mux.HandleFunc("GET /server/features", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"smartSearch": true, "facialRecognition": false})
	})
	mux.HandleFunc("GET /server/config", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"trashDays": 30, "userDeleteDelay": 7, "minFaces": 3, "isInitialized": true, "isOnboarded": true})
	})
	mux.HandleFunc("GET /server/storage", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"diskSizeRaw": 1000, "diskUseRaw": 400, "diskAvailableRaw": 600})
	})
	mux.HandleFunc("GET /server/statistics", func(w http.ResponseWriter, _ *http.Request) {
		if !isAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		writeJSON(w, map[string]any{
			"photos": 1000, "videos": 50, "usage": 12000, "usagePhotos": 9000, "usageVideos": 3000,
			"usageByUser": []map[string]any{{
				"userId": "u1", "userName": "admin", "photos": 1000, "videos": 50, "usage": 12000,
				"usagePhotos": 9000, "usageVideos": 3000, "quotaSizeInBytes": nil,
			}, {
				"userId": "u2", "userName": "bob", "photos": 10, "videos": 0, "usage": 900,
				"usagePhotos": 900, "usageVideos": 0, "quotaSizeInBytes": 1000,
			}},
		})
	})
	mux.HandleFunc("GET /admin/integrity/summary", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"checksum_mismatch": 0, "missing_file": 2, "untracked_file": 5})
	})
	mux.HandleFunc("GET /admin/database-backups", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"backups": []map[string]any{
			{"filename": "immich-db-backup-20261021T020000-v3.3.4-pg18.6.sql.gz", "filesize": 900, "timezone": "Asia/Shanghai"},
			{"filename": "immich-db-backup-20261022T020000-v3.3.4-pg18.6.sql.gz", "filesize": 1000, "timezone": "Asia/Shanghai"},
			{"filename": "uploaded-backup.sql.gz", "filesize": 50, "timezone": "Asia/Shanghai"},
		}})
	})
	mux.HandleFunc("GET /workflows", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{"enabled": true}, {"enabled": false}, {"enabled": true}})
	})
	mux.HandleFunc("GET /plugins", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{"name": "immich-core"}})
	})
	mux.HandleFunc("POST /search/statistics", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.UnmarshalRead(r.Body, &body)
		writeJSON(w, map[string]any{"total": searchStatsTotal(body)})
	})
	mux.HandleFunc("GET /assets/statistics", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"total": 3, "images": 2, "videos": 1})
	})
	mux.HandleFunc("GET /timeline/buckets", func(w http.ResponseWriter, r *http.Request) {
		// Guard: the exporter must keep sending size=MONTH (older servers require it).
		if r.URL.Query().Get("size") != "MONTH" {
			http.Error(w, "size is required", http.StatusBadRequest)
			return
		}
		writeJSON(w, []map[string]any{
			{"timeBucket": "2024-06-01", "count": 600},
			{"timeBucket": "2024-01-01", "count": 100},
			{"timeBucket": "2023-05-01", "count": 350},
		})
	})
	mux.HandleFunc("GET /search/suggestions", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("type") {
		case "camera-make":
			writeJSON(w, []string{"Apple", "Sony"})
		case "camera-model":
			writeJSON(w, []string{"iPhone 15 Pro", "A7 IV"})
		case "camera-lens-model":
			writeJSON(w, []string{"24mm"})
		case "city":
			writeJSON(w, []string{"Shanghai", "Tokyo", "Osaka"})
		case "state":
			writeJSON(w, []string{"Shanghai", "Tokyo"})
		case "country":
			writeJSON(w, []string{"China", "Japan"})
		default:
			writeJSON(w, []string{})
		}
	})
	mux.HandleFunc("GET /map/markers", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"country": "China", "city": "Shanghai", "lat": 31.0, "lon": 121.0},
			{"country": "China", "city": "", "lat": 31.4, "lon": 121.6},
			{"country": "Japan", "city": "Tokyo", "lat": 35.7, "lon": 139.7},
			{"country": "", "city": "", "lat": 0.0, "lon": 0.0},
		})
	})
	mux.HandleFunc("GET /people", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"people": []map[string]any{
				{"id": "p1", "name": "Alice"},
				{"id": "p2", "name": ""},
				{"id": "p3", "name": "Bob", "birthDate": "2000-01-01"},
			},
			"total": 40, "hidden": 2, "hasNextPage": false,
		})
	})
	mux.HandleFunc("GET /people/{id}/statistics", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"assets": 10})
	})
	mux.HandleFunc("GET /admin/users", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"id": "u1", "status": "active", "isAdmin": true},
			{"id": "u2", "status": "active", "isAdmin": false},
			{"id": "u3", "status": "deleted", "isAdmin": false},
		})
	})
	mux.HandleFunc("GET /admin/users/{id}/statistics", func(w http.ResponseWriter, r *http.Request) {
		var total float64
		if r.URL.Query().Get("visibility") == "locked" {
			total = map[string]float64{"u1": 2, "u2": 1}[r.PathValue("id")]
		}
		writeJSON(w, map[string]any{"total": total})
	})
	mux.HandleFunc("GET /queues", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{
				"name":       "smartSearch",
				"isPaused":   false,
				"statistics": map[string]any{"active": 1, "waiting": 12, "failed": 0, "delayed": 0, "completed": 5, "paused": 0},
			},
			{
				"name":       "thumbnailGeneration",
				"isPaused":   true,
				"statistics": map[string]any{"active": 0, "waiting": 0, "failed": 2, "delayed": 0, "completed": 100, "paused": 0},
			},
		})
	})
	mux.HandleFunc("GET /albums", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"id": "a1", "albumName": "Trips", "shared": true, "assetCount": 500, "hasSharedLink": true},
			{"id": "a2", "albumName": "Family", "shared": false, "assetCount": 0, "hasSharedLink": false},
		})
	})
	mux.HandleFunc("GET /albums/statistics", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"owned": 2, "shared": 1, "notShared": 1})
	})
	mux.HandleFunc("GET /shared-links", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"type": "ALBUM", "expiresAt": nil, "password": nil},
			{"type": "INDIVIDUAL", "expiresAt": "2000-01-01T00:00:00.000Z", "password": "x"},
		})
	})
	mux.HandleFunc("GET /partners", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("direction") == "shared-with" {
			writeJSON(w, []map[string]any{{}})
		} else {
			writeJSON(w, []map[string]any{{}, {}})
		}
	})
	mux.HandleFunc("GET /tags", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{"parentId": nil}, {"parentId": "x"}})
	})
	mux.HandleFunc("GET /memories/statistics", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"total": 7})
	})
	mux.HandleFunc("GET /libraries", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{"id": "l1", "name": "External", "assetCount": 0}})
	})
	mux.HandleFunc("GET /libraries/l1/statistics", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"photos": 4998, "videos": 2, "usage": 1000, "total": 5000})
	})
	mux.HandleFunc("GET /api-keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{}, {}})
	})
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{}})
	})
	mux.HandleFunc("GET /notifications", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{"level": "error", "readAt": nil}, {"level": "info", "readAt": "x"}})
	})
	mux.HandleFunc("GET /duplicates", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{"assets": []map[string]any{{}, {}}}})
	})
	mux.HandleFunc("GET /stacks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{"assets": []map[string]any{{}, {}, {}}}})
	})
	return mux
}

func testPoller(srv *httptest.Server) (*poller, *atomic.Pointer[snapshot]) {
	var snap atomic.Pointer[snapshot]
	var errs atomic.Int64
	p := &poller{
		c:    immich.New(srv.URL, "test", "immich-exporter/test", srv.Client()),
		snap: &snap, scrapeErrors: &errs,
		cfg: Config{
			Interval: time.Minute, BreakdownInterval: time.Minute,
			CollectCamera: true, CollectGeo: true, CollectRatings: true,
			CollectPeople: true, CollectHeavy: true,
			TopN: 25, FanoutLimit: 200, FanoutConcurrency: 4, CityLimit: 200,
		},
	}
	return p, &snap
}

func TestPollAdmin(t *testing.T) {
	srv := immichMock(t, true)
	p, snap := testPoller(srv)
	p.poll(context.Background())

	s := snap.Load()
	if s == nil {
		t.Fatal("nil snapshot")
	}
	eq := func(name string, got, want float64) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	eq("serverUp", s.health.serverUp, 1)
	eq("scrapeSuccess", s.health.scrapeSuccess, 1)
	eq("keyIsAdmin", s.health.keyIsAdmin, 1)
	eq("assets IMAGE", s.assets.byType["IMAGE"], 1000)
	eq("assets VIDEO", s.assets.byType["VIDEO"], 50)
	eq("serverUsageBytes", s.assets.serverUsageBytes.v, 12000)
	if len(s.users.perUser) != 2 || !s.users.perUser[0].quotaUnlimited || s.users.perUser[1].quotaBytes != 1000 {
		t.Errorf("perUser = %+v", s.users.perUser)
	}
	eq("favorite", s.assets.favorite.v, 12)
	eq("archived", s.assets.archived.v, 5)
	eq("offline", s.assets.offline.v, 2)
	eq("trashed", s.assets.trashed.v, 3)
	eq("locked", s.assets.locked.v, 3)
	eq("motion", s.assets.motion.v, 30)
	eq("notInAlbum", s.assets.notInAlbum.v, 400)
	eq("encoded", s.assets.encoded.v, 40)
	eq("rating unrated", s.assets.byRating["unrated"], 990)
	eq("rating 5", s.assets.byRating["5"], 5)
	eq("year 2024", s.assets.byYear["2024"], 700)
	eq("cameraMakes", s.cameras.makes.v, 2)
	eq("byMake Apple", s.cameras.byMake["Apple"], 800)
	eq("byModel iPhone", s.cameras.byModel["iPhone 15 Pro"], 700)
	eq("geotagged", s.geo.geotagged.v, 4)
	eq("country China", s.geo.byCountry["China"], 2)
	eq("country unknown", s.geo.byCountry["unknown"], 1)
	eq("countries", s.geo.countries.v, 2)
	if c := s.geo.countryCentroids["China"]; c.lat != "31.2" || c.lon != "121.3" {
		t.Errorf("China centroid = %v, want [31.2 121.3]", c)
	}
	if _, ok := s.geo.countryCentroids["unknown"]; ok {
		t.Error("unknown country should have no centroid")
	}
	eq("city Shanghai", s.geo.byCity[cityKey{city: "Shanghai", country: "China"}], 1)
	eq("city unknown/China", s.geo.byCity[cityKey{city: "unknown", country: "China"}], 1)
	eq("city Tokyo", s.geo.byCity[cityKey{city: "Tokyo", country: "Japan"}], 1)
	eq("city unknown/unknown", s.geo.byCity[cityKey{city: "unknown", country: "unknown"}], 1)
	if c := s.geo.cityCentroids[cityKey{city: "Shanghai", country: "China"}]; c.lat != "31.00" || c.lon != "121.00" {
		t.Errorf("Shanghai centroid = %v, want [31.00 121.00]", c)
	}
	if _, ok := s.geo.cityCentroids[cityKey{city: "unknown", country: "China"}]; ok {
		t.Error("unknown city should have no centroid")
	}
	eq("people", s.people.total, 40)
	eq("peopleNamed", s.people.named, 2)
	eq("peopleWithBirthdate", s.people.withBirthdate, 1)
	if len(s.people.assets) != 3 {
		t.Errorf("personAssets len = %d, want 3", len(s.people.assets))
	}
	eq("users active", s.users.byStatus["active"], 2)
	eq("users deleted", s.users.byStatus["deleted"], 1)
	eq("albums shared", s.albums.sharedCount, 1)
	eq("albums empty", s.albums.empty, 1)
	eq("album max", s.albums.assetsMax, 500)
	eq("sharedLinks ALBUM", s.albums.sharedLinks.byType["ALBUM"], 1)
	eq("sharedLinks expired", s.albums.sharedLinks.expired, 1)
	eq("partners incoming", s.albums.partners["incoming"], 1)
	eq("partners outgoing", s.albums.partners["outgoing"], 2)
	eq("tags", s.content.tags, 2)
	eq("tagsRoot", s.content.tagsRoot, 1)
	eq("memories", s.content.memories.v, 7)
	eq("duplicateSets", s.content.duplicateSets, 1)
	eq("duplicateAssets", s.content.duplicateAssets, 2)
	eq("stacks", s.content.stacks, 1)
	eq("stackedAssets", s.content.stackedAssets, 3)
	eq("libraries", s.content.libraries.v, 1)
	eq("storage size", s.server.storageSizeBytes.v, 1000)
	eq("serverLicensed", s.server.licensed, 1)
	eq("updateAvail", s.server.updateAvail, 1)
	if s.server.about.version != "v3.3.4" || s.server.latestVersion != "v3.3.5" {
		t.Errorf("version = %q, latest = %q", s.server.about.version, s.server.latestVersion)
	}
	eq("maintenance", s.server.maintenance.v, 0)
	eq("versionInstalled", s.server.versionInstalledUnix.v, float64(time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC).Unix()))
	if !s.server.versionCheckedUnix.ok {
		t.Error("versionChecked should be set from checkedAt")
	}
	eq("integrity missing_file", s.ops.integrity["missing_file"], 2)
	eq("integrity untracked_file", s.ops.integrity["untracked_file"], 5)
	eq("backups", s.ops.backups, 3)
	eq("backupsBytes", s.ops.backupsBytes, 1950)
	shanghai := time.FixedZone("CST", 8*60*60)
	eq("lastBackup", s.ops.lastBackupUnix.v, float64(time.Date(2026, 10, 22, 2, 0, 0, 0, shanghai).Unix()))
	eq("lastBackupBytes", s.ops.lastBackupBytes.v, 1000)
	eq("withPeople", s.metadata.withPeople.v, 600)
	eq("withCamera", s.metadata.withCamera.v, 900)
	if want := map[string]float64{
		"0-1MiB": 100, "1-5MiB": 600, "5-20MiB": 250, "20-100MiB": 50, "100MiB-1GiB": 40, "1GiB+": 4, "unknown": 6,
	}; !maps.Equal(s.metadata.bySize, want) {
		t.Errorf("bySize = %v, want %v", s.metadata.bySize, want)
	}
	eq("user photo bytes", s.users.perUser[0].photoBytes.v, 9000)
	eq("library usage", s.content.perLibraryBytes[0].value, 1000)
	eq("workflows enabled", s.content.workflows["true"], 2)
	eq("workflows disabled", s.content.workflows["false"], 1)
	eq("plugins", s.content.plugins.v, 1)
	for name, st := range s.health.collectors {
		if !st.ok {
			t.Errorf("collector %s failed", name)
		}
	}
	for _, name := range []string{"users/me", "jobs", "geo", "cameras", "duplicates", "assets/metadata", "people/statistics"} {
		if _, ok := s.health.collectors[name]; !ok {
			t.Errorf("collector %s missing from %v", name, slices.Collect(maps.Keys(s.health.collectors)))
		}
	}
	jobFound := map[string]float64{}
	for _, jq := range s.jobs.queues {
		jobFound[jq.queue+"/"+jq.state] = jq.count
	}
	eq("smartSearch waiting", jobFound["smartSearch/waiting"], 12)
	eq("smartSearch completed", jobFound["smartSearch/completed"], 5)
	eq("thumbnailGeneration failed", jobFound["thumbnailGeneration/failed"], 2)
	// comma-ok, not bare ==0: a missing key also reads 0 and would hide a dropped series.
	if v, ok := s.jobs.paused["smartSearch"]; !ok || v != 0 {
		t.Errorf("jobQueuePaused[smartSearch] = %v (present=%v), want 0 present", v, ok)
	}
	eq("thumbnailGeneration paused", s.jobs.paused["thumbnailGeneration"], 1)

	// End-to-end: the real poll output must emit through the collector cleanly.
	reg := prometheus.NewRegistry()
	reg.MustRegister(&collector{snap: snap, scrapeErrors: p.scrapeErrors, version: "t", goVersion: "go1"})
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather poll snapshot: %v", err)
	}
}

func TestPollNonAdminFallback(t *testing.T) {
	srv := immichMock(t, false)
	p, snap := testPoller(srv)
	p.poll(context.Background())

	s := snap.Load()
	if s == nil {
		t.Fatal("nil snapshot")
	}
	if s.health.keyIsAdmin != 0 {
		t.Errorf("keyIsAdmin = %v, want 0", s.health.keyIsAdmin)
	}
	// Asset counts come from the owner-scoped search/statistics fallback.
	if s.assets.byType["IMAGE"] != 1000 || s.assets.byType["VIDEO"] != 50 {
		t.Errorf("fallback assets = %+v", s.assets)
	}
	// Admin-only sections must be absent.
	if s.users.ok {
		t.Error("hasUsers should be false for non-admin key")
	}
	if len(s.jobs.queues) != 0 {
		t.Errorf("jobQueues should be empty for non-admin, got %d", len(s.jobs.queues))
	}
	if len(s.users.perUser) != 0 {
		t.Errorf("perUser should be empty for non-admin, got %d", len(s.users.perUser))
	}
	if s.health.scrapeSuccess != 1 {
		t.Errorf("scrapeSuccess = %v, want 1 (non-admin is not an error)", s.health.scrapeSuccess)
	}
}

// Only a fresh check may report a release: one older than the running server
// (seen live: v2.5.6 while running v3.3.3, the check disabled since March) or a
// checkedAt over a week old says nothing about what is available now.
func TestCollectVersionCheck(t *testing.T) {
	fresh := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	stale := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	cases := []struct {
		name, running, release, checkedAt string
		want                              float64
		latest                            bool // whether the release is reported at all
	}{
		{"newer release", "v3.0.1", "v3.1.0", fresh, 1, true},
		{"same version", "v3.1.0", "v3.1.0", fresh, 0, true},
		{"older release (check predates upgrade)", "v3.3.3", "v2.5.6", fresh, 0, false},
		{"stale check", "v3.0.1", "v3.1.0", stale, 0, false},
		{"no checkedAt (older server)", "v3.0.1", "v3.1.0", "", 1, true},
		{"numeric not lexicographic", "v3.9.0", "v3.10.0", fresh, 1, true},
		{"newer prerelease upstream", "v3.1.0", "v3.2.0-rc.1", fresh, 1, true},
		{"stable supersedes running prerelease", "v3.2.0-rc.1", "v3.2.0", fresh, 1, true},
		{"same prerelease", "v3.2.0-rc.1", "v3.2.0-rc.1", fresh, 0, true},
		{"prerelease of running stable", "v3.2.0", "v3.2.0-rc.1", fresh, 0, true},
		{"build metadata ignored", "v3.2.0+abc", "v3.2.0", fresh, 0, true},
		{"unparseable release", "v3.1.0", "nightly", fresh, 0, true},
		{"unknown running version", "", "v3.1.0", fresh, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /server/version-check", func(w http.ResponseWriter, _ *http.Request) {
				body := map[string]any{"releaseVersion": tc.release}
				if tc.checkedAt != "" {
					body["checkedAt"] = tc.checkedAt
				}
				writeJSON(w, body)
			})
			p, _ := testPoller(serve(t, mux))
			s := newSnapshot()
			s.server.about.version = tc.running
			if err := p.collectVersionCheck(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if s.server.updateAvail != tc.want {
				t.Errorf("updateAvail = %v, want %v", s.server.updateAvail, tc.want)
			}
			if got := s.server.latestVersion != ""; got != tc.latest {
				t.Errorf("latestVersion = %q, want reported=%v", s.server.latestVersion, tc.latest)
			}
			if s.server.versionCheckedUnix.ok != (tc.checkedAt != "") {
				t.Errorf("versionChecked present = %v, want %v", s.server.versionCheckedUnix.ok, tc.checkedAt != "")
			}
		})
	}
}

// A key scoped without a permission must not keep immich_exporter_up at 0; the
// refused collectors report 0 in immich_exporter_collector_success instead.
func TestPollRefusedPermissionIsNotAFailure(t *testing.T) {
	srv := serve(t, refuse(immichMux(true), http.StatusForbidden, "/albums", "/queues", "/jobs", "/people", "/admin/integrity/summary"))
	p, snap := testPoller(srv)
	p.poll(context.Background())
	s := snap.Load()
	if s.health.scrapeSuccess != 1 {
		t.Errorf("scrapeSuccess = %v, want 1", s.health.scrapeSuccess)
	}
	if n := p.scrapeErrors.Load(); n != 0 {
		t.Errorf("scrapeErrors = %d, want 0 for refusals", n)
	}
	for _, name := range []string{"albums", "jobs", "people", "integrity"} {
		if st, ok := s.health.collectors[name]; !ok || st.ok {
			t.Errorf("collector %s = %+v (present=%v), want failed", name, st, ok)
		}
	}
	if st := s.health.collectors["tags"]; !st.ok {
		t.Error("an unrelated collector should still succeed")
	}
}

// A 401 on users/me means the key itself is bad, so the poll fails and stops
// instead of logging the same rejection once per collector.
func TestPollInvalidKeyFails(t *testing.T) {
	var after atomic.Int32
	mux := immichMux(true)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server/ping", "/admin/maintenance/status":
			mux.ServeHTTP(w, r)
		case "/users/me":
			http.Error(w, `{"message":"Invalid API key"}`, http.StatusUnauthorized)
		default:
			after.Add(1)
			mux.ServeHTTP(w, r)
		}
	})
	p, snap := testPoller(serve(t, h))
	p.poll(context.Background())
	if s := snap.Load(); s.health.scrapeSuccess != 0 {
		t.Error("a 401 on users/me must fail the poll")
	}
	if n := after.Load(); n != 0 {
		t.Errorf("%d API calls after the key was rejected, want 0", n)
	}
}

func TestPollMaintenanceSkipsTheRest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/maintenance/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"active": true, "action": "restore_database"})
	})
	var other atomic.Int32
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		other.Add(1)
		http.Error(w, "maintenance", http.StatusServiceUnavailable)
	})
	p, snap := testPoller(serve(t, mux))
	p.poll(context.Background())
	s := snap.Load()
	if s.server.maintenance.v != 1 || s.health.scrapeSuccess != 1 {
		t.Errorf("maintenance = %v, scrapeSuccess = %v; want 1, 1", s.server.maintenance.v, s.health.scrapeSuccess)
	}
	// Only /server/ping may be called besides the status itself.
	if n := other.Load(); n > 1 {
		t.Errorf("%d other API calls during maintenance, want at most the ping", n)
	}
}

// Whole-library listings are slow-tier: within one breakdown interval a second
// poll must not fetch them again, yet still report their values.
func TestSlowTierCarriesForward(t *testing.T) {
	calls := map[string]*atomic.Int32{"/map/markers": {}, "/duplicates": {}, "/stacks": {}}
	mux := immichMux(true)
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := calls[r.URL.Path]; ok {
			c.Add(1)
		}
		mux.ServeHTTP(w, r)
	})
	p, snap := testPoller(serve(t, counted))
	p.cfg.BreakdownInterval = time.Hour
	p.poll(context.Background())
	p.poll(context.Background())
	for path, c := range calls {
		if n := c.Load(); n != 1 {
			t.Errorf("%s fetched %d times in two polls, want 1", path, n)
		}
	}
	s := snap.Load()
	if s.geo.geotagged.v != 4 || !s.content.duplicatesOK || s.content.stacks != 1 {
		t.Errorf("slow-tier values not carried forward: geotagged=%v duplicatesOK=%v stacks=%v",
			s.geo.geotagged.v, s.content.duplicatesOK, s.content.stacks)
	}
	if _, ok := s.health.collectors["geo"]; !ok {
		t.Error("slow-tier collector stats not carried forward")
	}
}

// Before Immich 3.2 the server strips StatisticsSearchDto.filter and would
// count every asset, so no filter body may be sent to it.
func TestSearchFilterNeedsImmich32(t *testing.T) {
	for v, want := range map[string]bool{
		"v3.2.0": true, "v3.3.5": true, "v4.0.0": true, "v3.2.0-rc.1": true,
		"v3.1.9": false, "v2.5.6": false, "": false, "nightly": false,
	} {
		if got := searchFilter(v); got != want {
			t.Errorf("searchFilter(%q) = %v, want %v", v, got, want)
		}
	}
	mux := immichMux(true)
	var filtered atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/server/about" {
			writeJSON(w, map[string]any{"version": "v3.1.0"})
			return
		}
		if r.URL.Path == "/search/statistics" {
			var body map[string]any
			_ = json.UnmarshalRead(r.Body, &body)
			if _, ok := body["filter"]; ok {
				filtered.Add(1)
			}
			writeJSON(w, map[string]any{"total": searchStatsTotal(body)})
			return
		}
		mux.ServeHTTP(w, r)
	})
	p, snap := testPoller(serve(t, h))
	p.poll(context.Background())
	if n := filtered.Load(); n != 0 {
		t.Errorf("%d filter queries sent to a v3.1 server", n)
	}
	if s := snap.Load(); s.metadata.withPeople.ok {
		t.Error("metadata counts must stay absent before Immich 3.2")
	}
}

func TestFoldCities(t *testing.T) {
	g := &geoSnap{
		byCity: map[cityKey]float64{
			{"Tokyo", "Japan"}: 50, {"Osaka", "Japan"}: 30, {"Kyoto", "Japan"}: 30, {"Nara", "Japan"}: 5, {"unknown", "Japan"}: 2,
		},
		cityCentroids: map[cityKey]latlon{{"Tokyo", "Japan"}: {"35.68", "139.69"}, {"Nara", "Japan"}: {"34.69", "135.80"}},
	}
	foldCities(g, 3)
	want := map[cityKey]float64{{"Tokyo", "Japan"}: 50, {"Kyoto", "Japan"}: 30, {"Osaka", "Japan"}: 30, {city: "other"}: 7}
	if !maps.Equal(g.byCity, want) {
		t.Errorf("byCity = %v, want %v", g.byCity, want)
	}
	if _, ok := g.cityCentroids[cityKey{"Nara", "Japan"}]; ok {
		t.Error("a folded city must drop its centroid")
	}
	foldCities(g, 0)
	if len(g.byCity) != 4 {
		t.Error("a limit of 0 must keep every city")
	}
}

func TestBackupTime(t *testing.T) {
	cases := []struct {
		name, zone string
		want       time.Time
		ok         bool
	}{
		{"immich-db-backup-20261022T020000-v3.3.3-pg18.6.sql.gz", "UTC", time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC), true},
		{"immich-db-backup-20261022T020000-v3.3.3-pg18.6.sql.gz", "America/New_York", time.Date(2026, 10, 22, 6, 0, 0, 0, time.UTC), true},
		{"uploaded-immich-db-backup-20250101T000000-v2.5.0-pg17.sql.gz", "nowhere/unknown", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"my-dump.sql", "UTC", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := backupTime(c.name, c.zone)
		if ok != c.ok || !got.Equal(c.want) {
			t.Errorf("backupTime(%q, %q) = %v, %v; want %v, %v", c.name, c.zone, got, ok, c.want, c.ok)
		}
	}
}

// Once a server drops the deprecated /server/features and /server/config, the
// same flags and settings come from /config (Immich 3.2+).
func TestFeaturesAndConfigFallBackToUserConfig(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"machineLearning": map[string]any{
				"enabled": true, "clip": map[string]any{"enabled": true}, "ocr": map[string]any{"enabled": false},
				"duplicateDetection": map[string]any{"enabled": true},
				"facialRecognition":  map[string]any{"enabled": true, "minFaces": 4},
			},
			"map": map[string]any{"enabled": true}, "reverseGeocoding": map[string]any{"enabled": true},
			"oauth": map[string]any{"enabled": false, "autoLaunch": false}, "passwordLogin": map[string]any{"enabled": true},
			"ffmpeg": map[string]any{"realtime": map[string]any{"enabled": false}},
			"trash":  map[string]any{"enabled": true, "days": 30}, "user": map[string]any{"deleteDelay": 7},
		})
	})
	p, _ := testPoller(serve(t, mux)) // /server/features and /server/config 404
	s := newSnapshot()
	if err := p.collectFeatures(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := p.collectConfig(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	f := s.server.features
	if f["smart_search"] != 1 || f["ocr"] != 0 || f["facial_recognition"] != 1 || f["password_login"] != 1 || f["oauth"] != 0 {
		t.Errorf("features = %v", f)
	}
	if _, ok := f["email"]; ok {
		t.Error("email has no /config counterpart and must stay absent")
	}
	if s.server.trashDays.v != 30 || s.server.userDeleteDelayDays.v != 7 || s.server.minFaces.v != 4 {
		t.Errorf("config = trash %v, delete delay %v, min faces %v", s.server.trashDays, s.server.userDeleteDelayDays, s.server.minFaces)
	}
	if s.server.initialized.ok || s.server.onboarded.ok {
		t.Error("initialized/onboarded have no /config counterpart and must stay absent")
	}
}

// assetStatesSrv serves just the endpoints collectAssetStates needs, optionally
// 403ing one visibility filter like a key without the matching permission.
func assetStatesSrv(t *testing.T, forbiddenVisibility string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /search/statistics", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.UnmarshalRead(r.Body, &body)
		if forbiddenVisibility != "" && body["visibility"] == forbiddenVisibility {
			http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
			return
		}
		writeJSON(w, map[string]any{"total": searchStatsTotal(body)})
	})
	mux.HandleFunc("GET /assets/statistics", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"total": 3})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCollectAssetStates(t *testing.T) {
	t.Run("all filters allowed", func(t *testing.T) {
		p, _ := testPoller(assetStatesSrv(t, ""))
		s := newSnapshot()
		if err := p.collectAssetStates(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		if !s.assets.archived.ok || s.assets.archived.v != 5 {
			t.Errorf("archived = %v (present=%v)", s.assets.archived.v, s.assets.archived.ok)
		}
		if !s.assets.motion.ok || s.assets.motion.v != 30 {
			t.Errorf("motion = %v (present=%v)", s.assets.motion.v, s.assets.motion.ok)
		}
	})

	t.Run("every filter refused fails", func(t *testing.T) {
		srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"Missing required permission"}`, http.StatusForbidden)
		}))
		p, _ := testPoller(srv)
		if err := p.collectAssetStates(context.Background(), newSnapshot()); !immich.StatusIs(err, http.StatusForbidden) {
			t.Errorf("err = %v, want the 403 rather than an empty success", err)
		}
	})

	t.Run("forbidden filter skips only its state", func(t *testing.T) {
		p, _ := testPoller(assetStatesSrv(t, "archive"))
		s := newSnapshot()
		if err := p.collectAssetStates(context.Background(), s); err != nil {
			t.Fatalf("a 403 on one filter must not fail the collector: %v", err)
		}
		if s.assets.archived.ok {
			t.Error("archived should be absent when its filter is forbidden")
		}
		// Every state after archived in the loop must still populate.
		if s.assets.hidden.v != 1 || s.assets.motion.v != 30 || s.assets.notInAlbum.v != 400 || s.assets.encoded.v != 40 || !s.assets.trashed.ok {
			t.Errorf("states after archived dropped: hidden=%v motion=%v notInAlbum=%v encoded=%v trashed(present)=%v",
				s.assets.hidden.v, s.assets.motion.v, s.assets.notInAlbum.v, s.assets.encoded.v, s.assets.trashed.ok)
		}
	})
}

func TestCollectSharedLinksNone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /shared-links", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p, _ := testPoller(srv)
	s := newSnapshot()
	if err := p.collectSharedLinks(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if want := map[string]float64{"ALBUM": 0, "INDIVIDUAL": 0}; !maps.Equal(s.albums.sharedLinks.byType, want) {
		t.Errorf("byType = %v, want %v", s.albums.sharedLinks.byType, want)
	}
}

// A key with library.read but not library.statistics must leave that library's
// sample absent (never a false zero) while the library count is still reported.
func TestCollectLibrariesStatisticsForbidden(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /libraries", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{"id": "l1", "name": "A"}, {"id": "l2", "name": "B"}})
	})
	mux.HandleFunc("GET /libraries/l1/statistics", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	mux.HandleFunc("GET /libraries/l2/statistics", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"total": 42})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p, _ := testPoller(srv)
	s := newSnapshot()
	if err := p.collectLibraries(context.Background(), s); err != nil {
		t.Fatalf("a 403 on one library's statistics must not fail the collector: %v", err)
	}
	if !s.content.libraries.ok || s.content.libraries.v != 2 {
		t.Errorf("libraries = %v (present=%v), want 2", s.content.libraries.v, s.content.libraries.ok)
	}
	if len(s.content.perLibrary) != 1 || s.content.perLibrary[0].id != "l2" || s.content.perLibrary[0].value != 42 {
		t.Errorf("perLibrary = %+v, want only l2=42", s.content.perLibrary)
	}
}

// collectJobs must fall back to the deprecated /jobs map when /queues is absent
// (older servers 404) or the key lacks queue.read (403).
func TestCollectJobsFallback(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden} {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /queues", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no queues here", status)
		})
		mux.HandleFunc("GET /jobs", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{
				"smartSearch": map[string]any{
					"jobCounts":   map[string]any{"active": 1, "waiting": 7, "failed": 0, "delayed": 0, "completed": 5, "paused": 0},
					"queueStatus": map[string]any{"isPaused": true},
				},
			})
		})
		srv := httptest.NewServer(mux)
		p, _ := testPoller(srv)
		s := newSnapshot()
		if err := p.collectJobs(context.Background(), s); err != nil {
			t.Fatalf("/queues %d should fall back to /jobs: %v", status, err)
		}
		var waiting float64
		var found bool
		for _, jq := range s.jobs.queues {
			if jq.queue == "smartSearch" && jq.state == "waiting" {
				waiting, found = jq.count, true
			}
		}
		if !found || waiting != 7 {
			t.Errorf("status %d: legacy smartSearch waiting = %v (found=%v), want 7", status, waiting, found)
		}
		if v, ok := s.jobs.paused["smartSearch"]; !ok || v != 1 {
			t.Errorf("status %d: legacy paused = %v (present=%v), want 1", status, v, ok)
		}
		srv.Close()
	}
}
