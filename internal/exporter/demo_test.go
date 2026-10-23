package exporter

import (
	"cmp"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// The demo library behind the dashboard screenshots (grafana/demo) is made up: a family whose
// phones back up every evening, who imported a camera card and a road trip during the month,
// and whose latest backup is still being processed when the screenshots are taken.

const (
	demoDays       = 30
	demoImageBytes = 3.7e6
	demoVideoBytes = 96e6
)

type demoPlace struct {
	city, country string
	lat, lon      float64
	assets        float64 // geotagged assets before the month started
	trip          float64 // share of the road trip taken here
}

var demoPlaces = []demoPlace{
	{"San Francisco", "United States", 37.7749, -122.4194, 7420, 0},
	{"Oakland", "United States", 37.8044, -122.2712, 1180, 0},
	{"Yosemite Valley", "United States", 37.7456, -119.5936, 640, 0},
	{"Los Angeles", "United States", 34.0522, -118.2437, 520, 0},
	{"New York", "United States", 40.7128, -74.006, 860, 0},
	{"Seattle", "United States", 47.6062, -122.3321, 410, 0},
	{"Honolulu", "United States", 21.3069, -157.8583, 690, 0},
	{"Tokyo", "Japan", 35.6762, 139.6503, 1640, 0},
	{"Kyoto", "Japan", 35.0116, 135.7681, 1120, 0},
	{"Osaka", "Japan", 34.6937, 135.5023, 380, 0},
	{"Sapporo", "Japan", 43.0618, 141.3545, 260, 0},
	{"Paris", "France", 48.8566, 2.3522, 980, 0},
	{"Nice", "France", 43.7102, 7.262, 310, 0},
	{"Rome", "Italy", 41.9028, 12.4964, 720, 0},
	{"Florence", "Italy", 43.7696, 11.2558, 540, 0},
	{"Venice", "Italy", 45.4408, 12.3155, 330, 0},
	{"Barcelona", "Spain", 41.3851, 2.1734, 610, 0},
	{"London", "United Kingdom", 51.5074, -0.1278, 590, 0},
	{"Edinburgh", "United Kingdom", 55.9533, -3.1883, 240, 0},
	{"Vancouver", "Canada", 49.2827, -123.1207, 420, 0},
	{"Banff", "Canada", 51.1784, -115.5708, 380, 0},
	{"Mexico City", "Mexico", 19.4326, -99.1332, 290, 0},
	{"Tulum", "Mexico", 20.2114, -87.4654, 210, 0},
	{"Sydney", "Australia", -33.8688, 151.2093, 450, 0},
	{"Melbourne", "Australia", -37.8136, 144.9631, 180, 0},
	{"Lisbon", "Portugal", 38.7223, -9.1393, 330, 0},
	{"Porto", "Portugal", 41.1579, -8.6291, 170, 0},
	{"Queenstown", "New Zealand", -45.0312, 168.6626, 260, 0},
	{"Bangkok", "Thailand", 13.7563, 100.5018, 220, 0},
	{"Chiang Mai", "Thailand", 18.7883, 98.9853, 140, 0},
	{"Tromsø", "Norway", 69.6492, 18.9553, 150, 0},
	{"Reykjavík", "Iceland", 64.1466, -21.9426, 160, 0.3},
	{"Vík", "Iceland", 63.4186, -19.006, 0, 0.25},
	{"Akureyri", "Iceland", 65.6885, -18.1262, 0, 0.2},
	{"Höfn", "Iceland", 64.2539, -15.2082, 0, 0.25},
}

// Shares of the library per camera make, model and lens.
var (
	demoMakes = map[string]float64{
		"Apple": 0.512, "Sony": 0.168, "Fujifilm": 0.083, "Google": 0.061, "Samsung": 0.042,
		"DJI": 0.026, "Canon": 0.021, "GoPro": 0.013, "Nikon": 0.007, "Ricoh": 0.004,
	}
	demoModels = map[string]float64{
		"iPhone 15 Pro": 0.198, "iPhone 13 Pro": 0.158, "ILCE-7M4": 0.108, "iPhone 11": 0.096,
		"ILCE-6400": 0.06, "iPhone XS": 0.06, "X-T5": 0.052, "SM-S918B": 0.042, "Pixel 8 Pro": 0.041,
		"X100VI": 0.031, "FC3582": 0.026, "Canon EOS R6": 0.021, "Pixel 6": 0.02,
		"HERO11 Black": 0.013, "NIKON Z 6_2": 0.007, "RICOH GR III": 0.004,
	}
	demoLenses = map[string]float64{
		"iPhone 15 Pro back triple camera 6.765mm f/1.78": 0.121,
		"iPhone 13 Pro back triple camera 5.7mm f/1.5":    0.098,
		"iPhone 11 back dual wide camera 4.25mm f/1.8":    0.071,
		"FE 24-70mm F2.8 GM II":                           0.064,
		"E 18-135mm F3.5-5.6 OSS":                         0.041,
		"FE 35mm F1.8":                                    0.033,
		"XF16-55mmF2.8 R LM WR":                           0.029,
		"Pixel 8 Pro back camera 6.9mm f/1.68":            0.027,
		"24.0 mm f/1.7":                                   0.022,
		"XF23mmF1.4 R LM WR":                              0.021,
		"FE 70-200mm F2.8 GM OSS II":                      0.016,
		"RF24-105mm F4 L IS USM":                          0.015,
		"NIKKOR Z 24-70mm f/4 S":                          0.006,
		"GR LENS 18.3mm F2.8":                             0.004,
	}
	// Assets taken in each of the twelve years before the current one.
	demoYears = []float64{380, 640, 910, 1150, 1560, 1970, 1650, 2280, 2830, 3060, 3190, 3260}
)

var demoPeople = []labeledVal{
	{"p01", "Emma", 3412},
	{"p02", "Liam", 2986},
	{"p03", "Olivia", 2140},
	{"p04", "Noah", 1876},
	{"p05", "Grandma Rose", 1204},
	{"p06", "Grandpa Joe", 1118},
	{"p07", "Ava", 964},
	{"p08", "Lucas", 812},
	{"p09", "Mia", 640},
	{"p10", "(unnamed)", 588},
	{"p11", "Sophia", 512},
	{"p12", "Ethan", 471},
	{"p13", "Chloe", 398},
	{"p14", "(unnamed)", 365},
	{"p15", "Daniel", 322},
}

var demoAlbums = []labeledVal{
	{"a01", "Japan 2025", 2350},
	{"a02", "Summer 2025", 1420},
	{"a03", "Wedding", 1180},
	{"a04", "Kids", 980},
	{"a05", "Italy 2024", 870},
	{"a06", "Hiking", 640},
	{"a07", "Christmas 2025", 410},
	{"a08", "Birthday Parties", 380},
	{"a09", "Pets", 360},
}

var demoUsers = []struct {
	id, name              string
	photos, videos, quota float64 // shares of the library's photos and videos; quota 0 is unlimited
}{
	{"u1", "Alex", 0.61, 0.58, 0},
	{"u2", "Sam", 0.26, 0.29, 750e9},
	{"u3", "Jamie", 0.09, 0.09, 300e9},
	{"u4", "Riley", 0.04, 0.04, 150e9},
}

type demoQueue struct {
	name              string
	perAsset, perHour float64 // jobs each upload enqueues, jobs the server finishes per hour
	workers, failed   float64
	backlog           float64
}

func newDemoQueues() []*demoQueue {
	return []*demoQueue{
		{name: "thumbnailGeneration", perAsset: 1, perHour: 1500, workers: 3, failed: 2},
		{name: "metadataExtraction", perAsset: 1, perHour: 2400, workers: 5},
		{name: "smartSearch", perAsset: 1, perHour: 320, workers: 2},
		{name: "faceDetection", perAsset: 1, perHour: 260, workers: 2},
		{name: "facialRecognition", perAsset: 0.6, perHour: 900, workers: 1},
		{name: "ocr", perAsset: 1, perHour: 300, workers: 1},
		{name: "duplicateDetection", perAsset: 1, perHour: 1800, workers: 1},
		{name: "videoConversion", perAsset: 0.07, perHour: 12, workers: 1, failed: 1},
		{name: "sidecar"},
		{name: "library"},
		{name: "backgroundTask"},
		{name: "storageTemplateMigration"},
		{name: "migration"},
		{name: "search"},
		{name: "notifications"},
		{name: "backupDatabase"},
	}
}

type demoTimeline struct {
	start, now time.Time
	home, trip []float64 // uploads in each hour since start
	queues     []*demoQueue
	last       time.Time
	lastAdded  float64
	r          *rand.Rand
}

func newDemoTimeline(now time.Time) *demoTimeline {
	r := rand.New(rand.NewPCG(2026, 10)) //nolint:gosec // a fixed seed keeps the demo reproducible.
	start := now.Add(-demoDays * 24 * time.Hour).Truncate(time.Hour)
	n := int(now.Sub(start).Hours()) + 1
	tl := &demoTimeline{
		start: start, now: now, home: make([]float64, n), trip: make([]float64, n),
		queues: newDemoQueues(), last: start, r: r,
	}
	for i := range n {
		if h := start.Add(time.Duration(i) * time.Hour).Hour(); h >= 18 && h <= 22 {
			tl.home[i] = float64(r.IntN(10))
		} else if r.IntN(5) == 0 {
			tl.home[i] = float64(r.IntN(3))
		}
	}
	tl.home[n-22*24] += 380
	tl.trip[n-12*24], tl.trip[n-12*24+1] = 610, 590
	return tl
}

// uploaded counts uploads from start to t, prorating the hour in progress. A phone backup starting
// twenty minutes before now keeps the queues busy while the screenshots render.
func (tl *demoTimeline) uploaded(t time.Time) (home, trip float64) {
	hours := t.Sub(tl.start).Hours()
	for i := range tl.home {
		f := min(1, hours-float64(i))
		if f <= 0 {
			break
		}
		home += tl.home[i] * f
		trip += tl.trip[i] * f
	}
	if m := t.Sub(tl.now).Minutes() + 20; m > 0 {
		home += 7 * m
	}
	return home, trip
}

func (tl *demoTimeline) scrapeErrors(t time.Time) int64 {
	switch {
	case t.After(tl.now.Add(-9 * 24 * time.Hour)):
		return 3
	case t.After(tl.now.Add(-23 * 24 * time.Hour)):
		return 1
	}
	return 0
}

// snapshotAt advances the queues to t, so it must be called with increasing times.
func (tl *demoTimeline) snapshotAt(t time.Time) *snapshot {
	home, trip := tl.uploaded(t)
	added := home + trip
	for _, q := range tl.queues {
		q.backlog = max(0, q.backlog+q.perAsset*(added-tl.lastAdded)-q.perHour*t.Sub(tl.last).Hours())
	}
	tl.last, tl.lastAdded = t, added

	images, videos := math.Round(24610+0.93*added), math.Round(1882+0.07*added)
	s := newSnapshot()
	s.health.serverUp, s.health.scrapeSuccess, s.health.keyIsAdmin = 1, 1, 1
	s.health.scrapeDurationSeconds = 1.1 + 0.6*tl.r.Float64()
	s.health.lastSuccessUnix = float64(t.Add(-41 * time.Second).Unix())
	demoCollectors(s, tl.r)
	demoServer(s, t, images, videos)
	demoOps(s, t)
	demoAssets(s, t.Year(), images, videos, home, trip)
	demoCameras(s, images+videos)
	demoGeo(s, home, trip)
	demoPeopleAt(s, home, trip)
	demoUsersAt(s, images, videos)
	demoAlbumsAt(s, home, trip)
	demoContent(s, trip)
	for _, q := range tl.queues {
		active := min(math.Ceil(q.backlog), q.workers)
		addQueue(s, q.name, false, queueCounts{Active: active, Waiting: max(0, math.Round(q.backlog)-active), Failed: q.failed})
	}
	return s
}

// demoCollectorCosts are the demo's collectors and their typical run time in seconds.
var demoCollectorCosts = map[string]float64{
	"maintenance": 0.004, "users/me": 0.006, "server/about": 0.005, "server/version-check": 0.004,
	"server/version-history": 0.005, "server/features": 0.004, "server/config": 0.004, "server/storage": 0.006,
	"assets": 0.09, "assets/state": 0.21, "assets/rating": 0.16, "timeline/buckets": 0.05, "people": 0.08,
	"admin/users": 0.01, "assets/locked": 0.03, "jobs": 0.02, "integrity": 0.01, "database-backups": 0.01, "albums": 0.04,
	"albums/statistics": 0.01, "shared-links": 0.01, "partners": 0.01, "tags": 0.01, "memories": 0.02,
	"libraries": 0.05, "api-keys": 0.01, "sessions": 0.01, "notifications": 0.01, "workflows": 0.01, "plugins": 0.01,
	"cameras": 0.92, "geo": 0.31, "people/statistics": 0.84, "duplicates": 0.38, "stacks": 0.03, "assets/metadata": 0.19,
}

func demoCollectors(s *snapshot, r *rand.Rand) {
	for _, name := range slices.Sorted(maps.Keys(demoCollectorCosts)) {
		s.health.collectors[name] = collectorStat{ok: true, seconds: demoCollectorCosts[name] * (0.8 + 0.4*r.Float64())}
	}
}

func demoServer(s *snapshot, t time.Time, images, videos float64) {
	sv := &s.server
	sv.about = aboutInfo{
		version: "v3.3.5", sourceRef: "v3.3.5", sourceCommit: "4b1c9e2", build: "18624409130",
		nodejs: "v24.21.0", exiftool: "13.59", ffmpeg: "7.1.4-3", imagemagick: "7.1.2-21", libvips: "8.18.7", ok: true,
	}
	sv.licensed, sv.latestVersion = 1, "v3.3.5"
	sv.maintenance = set(0)
	sv.versionCheckedUnix = set(float64(t.Add(-37 * time.Minute).Unix()))
	sv.versionInstalledUnix = set(float64(t.Add(-4*24*time.Hour - 7*time.Hour).Unix()))
	for _, f := range []string{"smart_search", "facial_recognition", "duplicate_detection", "map", "reverse_geocoding", "ocr", "sidecar", "search", "trash", "password_login", "email"} {
		sv.features[f] = 1
	}
	for _, f := range []string{"oauth", "oauth_auto_launch", "import_faces", "config_file"} {
		sv.features[f] = 0
	}
	sv.trashDays, sv.userDeleteDelayDays, sv.minFaces = set(30), set(7), set(3)
	sv.initialized, sv.onboarded = set(1), set(1)
	used := 1.31e12 + 1.12*(images*demoImageBytes+videos*demoVideoBytes)
	sv.storageSizeBytes, sv.storageUsedBytes, sv.storageAvailableBytes = set(4e12), set(used), set(4e12-used)
}

// demoOps keeps fourteen nightly backups, the newest from 02:00 today, and three files the integrity check flagged.
func demoOps(s *snapshot, t time.Time) {
	o := &s.ops
	o.integrity["checksum_mismatch"], o.integrity["missing_file"], o.integrity["untracked_file"] = 0, 0, 3
	last := time.Date(t.Year(), t.Month(), t.Day(), 2, 0, 0, 0, t.Location())
	if last.After(t) {
		last = last.AddDate(0, 0, -1)
	}
	o.backups, o.backupsBytes, o.backupsOK = 14, 14*47.2e6, true
	o.lastBackupUnix, o.lastBackupBytes = set(float64(last.Unix())), set(48.1e6)
}

func demoAssets(s *snapshot, year int, images, videos, home, trip float64) {
	total := images + videos
	m := &s.metadata
	m.withPeople, m.withCamera = set(math.Round(0.41*total)), set(math.Round(0.87*total))
	for size, share := range map[string]float64{
		"0-1MiB": 0.09, "1-5MiB": 0.58, "5-20MiB": 0.21, "20-100MiB": 0.08, "100MiB-1GiB": 0.035, "1GiB+": 0.005,
	} {
		m.bySize[size] = math.Round(share * total)
	}
	a := &s.assets
	a.byType["IMAGE"], a.byType["VIDEO"] = images, videos
	a.storageByType["IMAGE"], a.storageByType["VIDEO"] = images*demoImageBytes, videos*demoVideoBytes
	a.serverUsageBytes = set(images*demoImageBytes + videos*demoVideoBytes)
	rest := images + videos
	for i, n := range demoYears {
		a.byYear[strconv.Itoa(year-len(demoYears)+i)] = n
		rest -= n
	}
	a.byYear[strconv.Itoa(year)] = rest
	unrated := images + videos
	for stars, n := range map[string]float64{"5": 412, "4": 820, "3": 266, "2": 51, "1": 18} {
		a.byRating[stars] = n
		unrated -= n
	}
	a.byRating["unrated"] = unrated
	a.favorite = set(math.Round(1284 + 0.03*(home+trip)))
	a.archived, a.hidden, a.offline, a.trashed, a.locked = set(862), set(37), set(0), set(54), set(18)
	a.motion = set(math.Round(2214 + 0.11*home))
	a.notInAlbum = set(math.Round(9960 + 0.8*home + 0.1*trip))
	a.encoded = set(videos - 2)
}

func scaled(shares map[string]float64, total float64) map[string]float64 {
	m := make(map[string]float64, len(shares))
	for k, f := range shares {
		m[k] = math.Round(total * f)
	}
	return m
}

func demoCameras(s *snapshot, total float64) {
	c := &s.cameras
	c.makes, c.models, c.lenses = set(float64(len(demoMakes))), set(float64(len(demoModels))), set(float64(len(demoLenses)))
	c.byMake, c.byModel, c.byLens = scaled(demoMakes, total), scaled(demoModels, total), scaled(demoLenses, total)
}

func demoGeo(s *snapshot, home, trip float64) {
	g := &s.geo
	type weighted struct{ n, lat, lon float64 }
	countries := map[string]*weighted{}
	for _, p := range demoPlaces {
		n := p.assets + 0.97*p.trip*trip
		if p.city == "San Francisco" {
			n += 0.92 * home
		}
		if n = math.Round(n); n == 0 {
			continue
		}
		k := cityKey{p.city, p.country}
		g.byCity[k] = n
		g.cityCentroids[k] = latlon{strconv.FormatFloat(p.lat, 'f', 2, 64), strconv.FormatFloat(p.lon, 'f', 2, 64)}
		c := countries[p.country]
		if c == nil {
			c = &weighted{}
			countries[p.country] = c
		}
		c.n, c.lat, c.lon = c.n+n, c.lat+n*p.lat, c.lon+n*p.lon
	}
	var tagged float64
	for name, c := range countries {
		g.byCountry[name] = c.n
		g.countryCentroids[name] = latlon{strconv.FormatFloat(c.lat/c.n, 'f', 1, 64), strconv.FormatFloat(c.lon/c.n, 'f', 1, 64)}
		tagged += c.n
	}
	g.cities, g.countries, g.geotagged = set(float64(len(g.byCity))), set(float64(len(g.byCountry))), set(tagged)
	g.states = set(26)
	if trip > 0 {
		g.states = set(29)
	}
}

func demoPeopleAt(s *snapshot, home, trip float64) {
	newFaces := math.Round(0.0075 * trip)
	s.people = peopleSnap{
		total: 182 + newFaces, hidden: 11, named: 66, unnamed: 116 + newFaces, withBirthdate: 18, ok: true,
		assets: slices.Clone(demoPeople),
	}
	for i := range 4 { // the kids are in most new photos
		s.people.assets[i].value += math.Round(0.3*home + 0.45*trip)
	}
}

func demoUsersAt(s *snapshot, images, videos float64) {
	u := &s.users
	u.ok = true
	u.byStatus["active"] = float64(len(demoUsers))
	u.byRole["admin"], u.byRole["user"] = 1, float64(len(demoUsers)-1)
	for _, d := range demoUsers {
		photos, vids := math.Round(images*d.photos), math.Round(videos*d.videos)
		u.perUser = append(u.perUser, userStat{
			id: d.id, name: d.name, photos: photos, videos: vids,
			usageBytes: photos*demoImageBytes + vids*demoVideoBytes, quotaBytes: d.quota, quotaUnlimited: d.quota == 0,
			photoBytes: set(photos * demoImageBytes), videoBytes: set(vids * demoVideoBytes),
		})
	}
}

func demoAlbumsAt(s *snapshot, home, trip float64) {
	a := &s.albums
	top := slices.Clone(demoAlbums)
	top[3].value += math.Round(0.1 * home)
	shared, private, links := 11.0, 39.0, 5.0
	if trip > 0 {
		top = append(top, labeledVal{"a10", "Iceland Road Trip", math.Round(0.9 * trip)})
		private++
		links++
	}
	slices.SortFunc(top, func(x, y labeledVal) int { return cmp.Compare(y.value, x.value) })
	var inTop float64
	for _, al := range top {
		inTop += al.value
	}
	a.top = top
	a.owned, a.shared, a.notShared, a.statsOK = shared+private-3, shared, private, true
	a.sharedCount, a.privateCount, a.ok = shared, private, true
	a.assetsTotal = math.Round(1.35 * inTop)
	a.empty, a.withSharedLink = 2, links
	a.assetsMax, a.assetsAvg = top[0].value, math.Round(a.assetsTotal/(shared+private))
	sl := &a.sharedLinks
	sl.byType["ALBUM"], sl.byType["INDIVIDUAL"] = links+1, 3
	sl.expired, sl.neverExpire, sl.passwordProtected, sl.ok = 2, 4, 3, true
	a.partners["incoming"], a.partners["outgoing"] = 1, 1
}

func demoContent(s *snapshot, trip float64) {
	c := &s.content
	c.tags, c.tagsRoot, c.tagsOK = 58, 9, true
	c.memories = set(214)
	c.duplicateSets, c.duplicateAssets, c.duplicatesOK = 37, 81, true
	if trip > 0 {
		c.duplicateSets, c.duplicateAssets = 44, 96
	}
	c.stacks, c.stackedAssets, c.stacksOK = 128, 341, true
	c.libraries = set(2)
	c.perLibrary = []labeledVal{{"l1", "NAS Archive", 8420}, {"l2", "Film Scans", 1236}}
	c.perLibraryBytes = []labeledVal{{"l1", "NAS Archive", 8420 * 5.1e6}, {"l2", "Film Scans", 1236 * 31e6}}
	c.apiKeys, c.sessions = set(4), set(7)
	c.notifUnread, c.notifOK = 2, true
	c.notifByLevel["info"], c.notifByLevel["success"], c.notifByLevel["warning"], c.notifByLevel["error"] = 6, 3, 1, 1
	c.workflows["true"], c.workflows["false"] = 2, 1
	c.plugins = set(1)
}

func demoRegistry(snap *atomic.Pointer[snapshot], errs *atomic.Int64) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(&collector{snap: snap, scrapeErrors: errs, version: "demo", goVersion: "go-demo"})
	return reg
}

// Every exported metric needs a demo value, or its panels show No data in the screenshots.
func TestDemoCoversEveryMetric(t *testing.T) {
	var snap atomic.Pointer[snapshot]
	var errs atomic.Int64
	now := time.Now()
	snap.Store(newDemoTimeline(now).snapshotAt(now))
	mfs, err := demoRegistry(&snap, &errs).Gather()
	if err != nil {
		t.Fatal(err)
	}
	demo := map[string]bool{}
	for _, mf := range mfs {
		demo[mf.GetName()] = true
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "metrics.golden"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(raw)) {
		if f := strings.Fields(line); len(f) >= 3 && f[0] == "#" && f[1] == "TYPE" && !demo[f[2]] {
			t.Errorf("%s has no demo value; add one in demo_test.go", f[2])
		}
	}
}

// TestDemoMetrics writes the demo history, hourly for a month and then every minute, as timestamped
// Prometheus text for grafana/demo/screenshots.sh to import.
func TestDemoMetrics(t *testing.T) {
	out := os.Getenv("DEMO_METRICS")
	if out == "" {
		t.Skip("set DEMO_METRICS to the file to write")
	}
	now := time.Now()
	tl := newDemoTimeline(now)
	var snap atomic.Pointer[snapshot]
	var errs atomic.Int64
	reg := demoRegistry(&snap, &errs)
	var b, family strings.Builder
	write := func(ts time.Time) {
		snap.Store(tl.snapshotAt(ts))
		errs.Store(tl.scrapeErrors(ts))
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		ms := ts.UnixMilli()
		for _, mf := range mfs {
			for _, m := range mf.GetMetric() {
				m.TimestampMs = &ms
			}
			family.Reset()
			if _, err := expfmt.MetricFamilyToText(&family, mf); err != nil {
				t.Fatal(err)
			}
			for line := range strings.Lines(family.String()) {
				if !strings.HasPrefix(line, "#") {
					b.WriteString(line)
				}
			}
		}
	}
	minutes := now.Add(-3 * time.Hour).Truncate(time.Minute)
	for ts := tl.start; ts.Before(minutes); ts = ts.Add(time.Hour) {
		write(ts)
	}
	// Minutes after now keep instant queries answered while the screenshots render.
	for ts := minutes; !ts.After(now.Add(15 * time.Minute)); ts = ts.Add(time.Minute) {
		write(ts)
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o600); err != nil { //nolint:gosec // out is the DEMO_METRICS path the developer chose.
		t.Fatal(err)
	}
}
