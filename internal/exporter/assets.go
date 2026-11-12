package exporter

import (
	"context"
	"maps"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/moond4rk/immich-exporter/internal/immich"
)

// metadataSnap counts assets through StatisticsSearchDto.filter, which only Immich 3.2+ understands.
type metadataSnap struct {
	withPeople, withCamera opt
	bySize                 map[string]float64 // file-size bucket -> count
}

type assetsSnap struct {
	byType           map[string]float64 // AssetTypeEnum -> count (instance-wide with admin, owner-scoped otherwise)
	storageByType    map[string]float64 // AssetTypeEnum -> bytes
	serverUsageBytes opt
	byYear           map[string]float64
	byRating         map[string]float64 // "1".."5","unrated"
	visible          opt                // the base of the owner-scoped counts below
	favorite, archived, hidden, offline,
	motion, notInAlbum, encoded, trashed opt
	locked opt // instance-wide, unlike the owner-scoped states above
}

var (
	// assets
	dAssets       = groupAssets.desc("assets", "Number of assets by type: every user's with an admin key, the key owner's otherwise.", "type")
	dAssetStorage = groupAssets.desc("assets_storage_bytes", "Storage used by assets by type, in bytes (admin).", "type")
	dServerUsage  = groupAssets.desc("server_usage_bytes", "Total bytes used by all original assets server-wide (admin).")
	dAssetsByYear = groupAssets.desc("assets_by_year", "Assets in the timeline by the year they were taken.", "year")
	dAssetsByRate = groupAssets.desc("assets_by_rating", "Assets by star rating (1-5, or 'unrated').", "rating")
	dVisible      = groupAssets.desc("assets_visible", "Assets in the API key owner's timeline and archive, which leaves out the trash and hidden and locked assets. The counts that come from Immich's search are counted among these.")
	dFavorite     = groupAssets.desc("assets_favorite", "Number of favourited assets.")
	dArchived     = groupAssets.desc("assets_archived", "Number of archived assets.")
	dHidden       = groupAssets.desc("assets_hidden", "Number of hidden assets, including the video halves of live photos.")
	dLocked       = groupAssets.desc("assets_locked", "Assets in users' PIN-locked folders, summed over every user (admin).")
	dOffline      = groupAssets.desc("assets_offline", "Number of offline assets (file missing on disk).")
	dMotion       = groupAssets.desc("assets_motion", "Number of motion / live-photo assets.")
	dNotInAlbum   = groupAssets.desc("assets_not_in_album", "Number of assets not assigned to any album.")
	dEncoded      = groupAssets.desc("assets_encoded", "Number of transcoded video assets.")
	dTrashed      = groupAssets.desc("assets_trashed", "Number of trashed assets.")
	dWithPeople   = groupAssets.desc("assets_with_people", "Visible assets (timeline and archive) showing at least one recognized person (Immich 3.2+).")
	dWithCamera   = groupAssets.desc("assets_with_camera", "Visible assets (timeline and archive) whose EXIF names a camera make (Immich 3.2+).")
	dAssetsBySize = groupAssets.desc("assets_by_file_size", "Visible assets (timeline and archive) by original file size (Immich 3.2+).", "size")
)

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

func (p *poller) collectAssets(ctx context.Context, s *snapshot) error {
	if s.health.keyIsAdmin.v == 1 {
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
		n, err := p.count(ctx, where("type", t))
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
	checks := []struct {
		v *opt
		q query
	}{
		{&a.visible, query{flat: map[string]any{}, filter: visible(nil)}},
		{&a.favorite, where("isFavorite", true)},
		{&a.archived, where("visibility", "archive")},
		{&a.hidden, where("visibility", "hidden")},
		// Immich moves an offline asset to the trash, so this one count keeps the trash.
		{&a.offline, query{
			flat:   map[string]any{"isOffline": true},
			filter: map[string]any{"isOffline": eq(true), "visibility": browsable},
		}},
		{&a.motion, where("isMotion", true)},
		{&a.notInAlbum, query{
			flat:   map[string]any{"isNotInAlbum": true},
			filter: visible(map[string]any{"hasAlbums": eq(false)}),
		}},
		{&a.encoded, where("isEncoded", true)},
	}
	// A filter the API key may not query must not drop the other states, but
	// refusing all of them is a failure, not an empty success.
	var refused error
	for _, c := range checks {
		n, err := p.count(ctx, c.q)
		if err != nil {
			if isSoftStatus(err) {
				refused = err
				continue
			}
			return err
		}
		*c.v = set(n)
	}
	if !p.filter {
		// The flat total counts hidden assets too, so it is only the visible total once they are taken off.
		if a.hidden.ok {
			a.visible.v -= a.hidden.v
		} else {
			a.visible = opt{}
		}
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
	for _, v := range []opt{a.visible, a.favorite, a.archived, a.hidden, a.offline, a.motion, a.notInAlbum, a.encoded, a.trashed} {
		if v.ok {
			return nil
		}
	}
	return refused
}

func (p *poller) collectRatings(ctx context.Context, s *snapshot) error {
	for r := 1; r <= 5; r++ {
		n, err := p.count(ctx, where("rating", r))
		if err != nil {
			return err
		}
		s.assets.byRating[strconv.Itoa(r)] = n
	}
	n, err := p.count(ctx, where("rating", nil))
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

// searchFilter reports whether the server reads StatisticsSearchDto.filter
// (Immich 3.2+). An older one strips the unknown field and counts every asset.
func searchFilter(version string) bool {
	v, _, ok := parseSemver(version)
	return ok && !semverGreater([3]int{3, 2, 0}, v)
}

// query is one asset count in both search dialects: Immich 4 drops the flat fields, and before 3.2 ignores the filter.
type query struct {
	flat   map[string]any
	filter map[string]any
}

func eq(v any) map[string]any { return map[string]any{"eq": v} }

var browsable = map[string]any{"in": []string{"timeline", "archive"}}

// visible leaves out what a search counts but nobody browses: the trash and the hidden video half of each live photo.
func visible(filter map[string]any) map[string]any {
	f := map[string]any{"visibility": browsable, "trashedAt": eq(nil)}
	maps.Copy(f, filter)
	return f
}

func where(field string, v any) query {
	return query{flat: map[string]any{field: v}, filter: visible(map[string]any{field: eq(v)})}
}

func (p *poller) count(ctx context.Context, q query) (float64, error) {
	if p.filter {
		return p.c.StatTotal(ctx, map[string]any{"filter": q.filter})
	}
	return p.c.StatTotal(ctx, q.flat)
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
	count := func(filter map[string]any) (float64, error) {
		return p.c.StatTotal(ctx, map[string]any{"filter": visible(filter)})
	}
	n, err := count(map[string]any{"hasPeople": map[string]any{"eq": true}})
	if err != nil {
		return err
	}
	m.withPeople = set(n)
	if n, err = count(map[string]any{"make": map[string]any{"ne": nil}}); err != nil {
		return err
	}
	m.withCamera = set(n)

	total, err := count(nil)
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

func emitAssets(ch chan<- prometheus.Metric, s *snapshot) {
	m := &s.metadata
	emitOpt(ch, dWithPeople, m.withPeople)
	emitOpt(ch, dWithCamera, m.withCamera)
	for size, v := range m.bySize {
		emit(ch, dAssetsBySize, v, size)
	}
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
	emitOpt(ch, dVisible, a.visible)
	emitOpt(ch, dFavorite, a.favorite)
	emitOpt(ch, dArchived, a.archived)
	emitOpt(ch, dHidden, a.hidden)
	emitOpt(ch, dOffline, a.offline)
	emitOpt(ch, dMotion, a.motion)
	emitOpt(ch, dNotInAlbum, a.notInAlbum)
	emitOpt(ch, dEncoded, a.encoded)
	emitOpt(ch, dTrashed, a.trashed)
	emitOpt(ch, dLocked, a.locked)
}
