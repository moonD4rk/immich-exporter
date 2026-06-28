package exporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/moond4rk/immich-exporter/internal/immich"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
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
	case m["visibility"] == "locked":
		return 0
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
	return 0
}

func immichMock(t *testing.T, isAdmin bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /server/ping", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"res": "pong"})
	})
	mux.HandleFunc("GET /users/me", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"isAdmin": isAdmin})
	})
	mux.HandleFunc("GET /server/about", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"version": "v1.120.0", "ffmpeg": "6.0", "licensed": true})
	})
	mux.HandleFunc("GET /server/version-check", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"releaseVersion": "v1.121.0"})
	})
	mux.HandleFunc("GET /server/features", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"smartSearch": true, "facialRecognition": false})
	})
	mux.HandleFunc("GET /server/config", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"trashDays": 30, "userDeleteDelay": 7, "isOnboarded": true})
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
			"usageByUser": []map[string]any{
				{"userId": "u1", "userName": "admin", "photos": 1000, "videos": 50, "usage": 12000, "quotaSizeInBytes": nil},
			},
		})
	})
	mux.HandleFunc("POST /search/statistics", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, map[string]any{"total": searchStatsTotal(body)})
	})
	mux.HandleFunc("GET /assets/statistics", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"total": 3, "images": 2, "videos": 1})
	})
	mux.HandleFunc("GET /timeline/buckets", func(w http.ResponseWriter, _ *http.Request) {
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
			{"country": "China", "lat": 31.0, "lon": 121.0},
			{"country": "China", "lat": 31.4, "lon": 121.6},
			{"country": "Japan", "lat": 35.7, "lon": 139.7},
			{"country": "", "lat": 0.0, "lon": 0.0},
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
			{"status": "active", "isAdmin": true},
			{"status": "active", "isAdmin": false},
			{"status": "deleted", "isAdmin": false},
		})
	})
	mux.HandleFunc("GET /jobs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"smartSearch": map[string]any{
				"jobCounts":   map[string]any{"active": 1, "waiting": 12, "failed": 0, "delayed": 0, "completed": 5, "paused": 0},
				"queueStatus": map[string]any{"isActive": true, "isPaused": false},
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
		writeJSON(w, []map[string]any{{"id": "l1", "name": "External", "assetCount": 5000}})
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
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testPoller(srv *httptest.Server) (*poller, *atomic.Pointer[snapshot]) {
	var snap atomic.Pointer[snapshot]
	var errs atomic.Int64
	p := &poller{
		c:    immich.New(srv.URL, "test", srv.Client()),
		snap: &snap, scrapeErrors: &errs,
		cfg: Config{
			Interval: time.Minute, BreakdownInterval: time.Minute,
			CollectCamera: true, CollectGeo: true, CollectRatings: true,
			CollectPeople: true, CollectHeavy: true,
			TopN: 25, FanoutLimit: 200, FanoutConcurrency: 4,
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
	eq("serverUp", s.serverUp, 1)
	eq("scrapeSuccess", s.scrapeSuccess, 1)
	eq("keyIsAdmin", s.keyIsAdmin, 1)
	eq("assets IMAGE", s.assets["IMAGE"], 1000)
	eq("assets VIDEO", s.assets["VIDEO"], 50)
	eq("serverUsageBytes", s.serverUsageBytes, 12000)
	if len(s.perUser) != 1 || !s.perUser[0].quotaUnlimited {
		t.Errorf("perUser = %+v", s.perUser)
	}
	eq("favorite", s.assetsFavorite, 12)
	eq("archived", s.assetsArchived, 5)
	eq("offline", s.assetsOffline, 2)
	eq("trashed", s.assetsTrashed, 3)
	eq("rating unrated", s.assetsByRating["unrated"], 990)
	eq("rating 5", s.assetsByRating["5"], 5)
	eq("year 2024", s.assetsByYear["2024"], 700)
	eq("cameraMakes", s.cameraMakes, 2)
	eq("byMake Apple", s.assetsByMake["Apple"], 800)
	eq("byModel iPhone", s.assetsByModel["iPhone 15 Pro"], 700)
	eq("geotagged", s.geotagged, 4)
	eq("country China", s.assetsByCountry["China"], 2)
	eq("country unknown", s.assetsByCountry["unknown"], 1)
	eq("countries", s.countries, 2)
	if c := s.geoCentroids["China"]; c[0] != "31.2" || c[1] != "121.3" {
		t.Errorf("China centroid = %v, want [31.2 121.3]", c)
	}
	if _, ok := s.geoCentroids["unknown"]; ok {
		t.Error("unknown country should have no centroid")
	}
	eq("people", s.people, 40)
	eq("peopleNamed", s.peopleNamed, 2)
	eq("peopleWithBirthdate", s.peopleWithBirthdate, 1)
	if len(s.personAssets) != 3 {
		t.Errorf("personAssets len = %d, want 3", len(s.personAssets))
	}
	eq("users active", s.usersByStatus["active"], 2)
	eq("users deleted", s.usersByStatus["deleted"], 1)
	eq("albums shared", s.albumsSharedCount, 1)
	eq("albums empty", s.albumsEmpty, 1)
	eq("album max", s.albumAssetsMax, 500)
	eq("sharedLinks ALBUM", s.sharedLinks["ALBUM"], 1)
	eq("sharedLinks expired", s.sharedLinksExpired, 1)
	eq("partners incoming", s.partners["incoming"], 1)
	eq("partners outgoing", s.partners["outgoing"], 2)
	eq("tags", s.tags, 2)
	eq("tagsRoot", s.tagsRoot, 1)
	eq("memories", s.memories, 7)
	eq("duplicateSets", s.duplicateSets, 1)
	eq("duplicateAssets", s.duplicateAssets, 2)
	eq("stacks", s.stacks, 1)
	eq("stackedAssets", s.stackedAssets, 3)
	eq("libraries", s.libraries, 1)
	eq("storage size", s.storageSizeBytes, 1000)
	eq("serverLicensed", s.serverLicensed, 1)
	eq("updateAvail", s.updateAvail, 1)
	if s.serverInfo["version"] != "v1.120.0" {
		t.Errorf("serverInfo version = %q", s.serverInfo["version"])
	}
	var jobOK bool
	for _, jq := range s.jobQueues {
		if jq.queue == "smartSearch" && jq.state == "waiting" && jq.count == 12 {
			jobOK = true
		}
	}
	if !jobOK {
		t.Errorf("smartSearch waiting=12 not found in %+v", s.jobQueues)
	}

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
	if s.keyIsAdmin != 0 {
		t.Errorf("keyIsAdmin = %v, want 0", s.keyIsAdmin)
	}
	// Asset counts come from the owner-scoped search/statistics fallback.
	if s.assets["IMAGE"] != 1000 || s.assets["VIDEO"] != 50 {
		t.Errorf("fallback assets = %+v", s.assets)
	}
	// Admin-only sections must be absent.
	if s.hasUsers {
		t.Error("hasUsers should be false for non-admin key")
	}
	if len(s.jobQueues) != 0 {
		t.Errorf("jobQueues should be empty for non-admin, got %d", len(s.jobQueues))
	}
	if len(s.perUser) != 0 {
		t.Errorf("perUser should be empty for non-admin, got %d", len(s.perUser))
	}
	if s.scrapeSuccess != 1 {
		t.Errorf("scrapeSuccess = %v, want 1 (non-admin is not an error)", s.scrapeSuccess)
	}
}
