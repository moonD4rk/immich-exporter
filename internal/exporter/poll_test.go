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
		// The PIN-protected locked folder rejects API-key access on real Immich;
		// this must not abort the other per-state counts.
		if body["visibility"] == "locked" {
			http.Error(w, `{"message":"Elevated permission is required"}`, http.StatusUnauthorized)
			return
		}
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
			{"status": "active", "isAdmin": true},
			{"status": "active", "isAdmin": false},
			{"status": "deleted", "isAdmin": false},
		})
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
	eq("serverUp", s.health.serverUp, 1)
	eq("scrapeSuccess", s.health.scrapeSuccess, 1)
	eq("keyIsAdmin", s.health.keyIsAdmin, 1)
	eq("assets IMAGE", s.assets.byType["IMAGE"], 1000)
	eq("assets VIDEO", s.assets.byType["VIDEO"], 50)
	eq("serverUsageBytes", s.assets.serverUsageBytes.v, 12000)
	if len(s.users.perUser) != 1 || !s.users.perUser[0].quotaUnlimited {
		t.Errorf("perUser = %+v", s.users.perUser)
	}
	eq("favorite", s.assets.favorite.v, 12)
	eq("archived", s.assets.archived.v, 5)
	eq("offline", s.assets.offline.v, 2)
	eq("trashed", s.assets.trashed.v, 3)
	// locked visibility 401s (PIN-protected) — it must be skipped without
	// dropping the states that come after it in the loop.
	if s.assets.locked.ok {
		t.Error("assetsLocked should be absent when locked visibility is forbidden")
	}
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
	if s.server.about.version != "v1.120.0" {
		t.Errorf("serverInfo version = %q", s.server.about.version)
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

// version-check can return a release OLDER than the running server (seen live:
// v2.5.6 reported while running v3.0.1); only strictly-newer may set the flag.
func TestCollectVersionCheck(t *testing.T) {
	cases := []struct {
		name, running, release string
		want                   float64
	}{
		{"newer release", "v3.0.1", "v3.1.0", 1},
		{"same version", "v3.1.0", "v3.1.0", 0},
		{"older release (lagging check)", "v3.0.1", "v2.5.6", 0},
		{"numeric not lexicographic", "v3.9.0", "v3.10.0", 1},
		{"prerelease suffix ignored", "v3.1.0", "v3.2.0-rc.1", 1},
		{"unparseable release", "v3.1.0", "nightly", 0},
		{"unknown running version", "", "v3.1.0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /server/version-check", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, map[string]any{"releaseVersion": tc.release})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			p, _ := testPoller(srv)
			s := newSnapshot()
			s.server.about.version = tc.running
			if err := p.collectVersionCheck(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if s.server.updateAvail != tc.want {
				t.Errorf("updateAvail = %v, want %v", s.server.updateAvail, tc.want)
			}
			if s.server.latestVersion != tc.release {
				t.Errorf("latestVersion = %q, want %q", s.server.latestVersion, tc.release)
			}
		})
	}
}

// assetStatesSrv serves just the endpoints collectAssetStates needs, optionally
// 401ing the locked-visibility query like a real PIN-protected folder.
func assetStatesSrv(t *testing.T, lockedForbidden bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /search/statistics", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if lockedForbidden && body["visibility"] == "locked" {
			http.Error(w, `{"message":"Elevated permission is required"}`, http.StatusUnauthorized)
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
	t.Run("locked accessible", func(t *testing.T) {
		p, _ := testPoller(assetStatesSrv(t, false))
		s := newSnapshot()
		if err := p.collectAssetStates(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		if !s.assets.locked.ok {
			t.Error("hasLocked should be true when the key can query locked visibility")
		}
		if !s.assets.motion.ok || s.assets.motion.v != 30 {
			t.Errorf("motion = %v (present=%v)", s.assets.motion.v, s.assets.motion.ok)
		}
	})

	t.Run("locked 401 skips only locked", func(t *testing.T) {
		p, _ := testPoller(assetStatesSrv(t, true))
		s := newSnapshot()
		if err := p.collectAssetStates(context.Background(), s); err != nil {
			t.Fatalf("a 401 on locked must not fail the collector: %v", err)
		}
		if s.assets.locked.ok {
			t.Error("hasLocked should be false when locked visibility is forbidden")
		}
		// Every state after locked in the loop must still populate.
		if s.assets.motion.v != 30 || s.assets.notInAlbum.v != 400 || s.assets.encoded.v != 40 || !s.assets.trashed.ok {
			t.Errorf("states after locked dropped: motion=%v notInAlbum=%v encoded=%v trashed(present)=%v",
				s.assets.motion.v, s.assets.notInAlbum.v, s.assets.encoded.v, s.assets.trashed.ok)
		}
	})
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
