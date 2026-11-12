package exporter

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

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

var (
	// albums / sharing
	dAlbums          = groupAlbums.desc("albums", "Number of albums by sharing state.", "shared")
	dAlbumsOwned     = groupAlbums.desc("albums_owned", "Number of albums owned by the API key user.")
	dAlbumsSharedSt  = groupAlbums.desc("albums_shared", "Number of albums shared (from /albums/statistics).")
	dAlbumsNotShared = groupAlbums.desc("albums_not_shared", "Number of private (not shared) albums.")
	dAlbumAssets     = groupAlbums.desc("album_assets", "Total asset memberships across all albums (assets may repeat).")
	dAlbumsEmpty     = groupAlbums.desc("albums_empty", "Number of albums containing no assets.")
	dAlbumsWithLink  = groupAlbums.desc("albums_with_shared_link", "Number of albums exposing a public shared link.")
	dAlbumMax        = groupAlbums.desc("album_assets_max", "Asset count of the largest album.")
	dAlbumAvg        = groupAlbums.desc("album_assets_avg", "Average asset count per album.")
	dTopAlbum        = groupAlbums.desc("album_top_assets", "Asset count of the top-N largest albums.", "album_id", "album")

	dSharedLinks      = groupAlbums.desc("shared_links", "Number of shared links by type.", "type")
	dSharedLinksExp   = groupAlbums.desc("shared_links_expired", "Number of expired shared links still present.")
	dSharedLinksNever = groupAlbums.desc("shared_links_never_expire", "Number of shared links with no expiry.")
	dSharedLinksPwd   = groupAlbums.desc("shared_links_password_protected", "Number of password-protected shared links.")
	dPartners         = groupAlbums.desc("partners", "Number of partner shares by direction (incoming/outgoing).", "direction")
)

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
