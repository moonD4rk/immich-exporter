package exporter

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// userStat holds the per-user volumetrics from /server/statistics.
type userStat struct {
	id, name               string
	photos, videos         float64
	usageBytes             float64
	photoBytes, videoBytes opt
	quotaBytes             float64
	quotaUnlimited         bool
}

// userAssets holds one user's asset states from the admin statistics.
type userAssets struct {
	id, name                            string
	favorite, archived, trashed, locked float64
}

type usersSnap struct {
	byStatus, byRole map[string]float64
	ok               bool
	perUser          []userStat
	assets           []userAssets
}

var (
	// users
	dUsers       = groupUsers.desc("users", "Number of users by status.", "status")
	dUsersByRole = groupUsers.desc("users_by_role", "Number of users by role.", "role")
	dUserPhotos  = groupUsers.desc("user_photos", "Per-user photo count.", "user_id", "user")
	dUserVideos  = groupUsers.desc("user_videos", "Per-user video count.", "user_id", "user")
	dUserUsage   = groupUsers.desc("user_usage_bytes", "Per-user total storage usage, in bytes.", "user_id", "user")
	dUserStorage = groupUsers.desc("user_storage_bytes", "Per-user storage usage by asset type, in bytes.", "user_id", "user", "type")
	dUserQuota   = groupUsers.desc("user_quota_bytes", "Per-user storage quota in bytes (omitted when unlimited).", "user_id", "user")
	dUserUnlim   = groupUsers.desc("user_quota_unlimited", "1 if the user has no storage quota (unlimited).", "user_id", "user")

	dUserFavorite = groupUsers.desc("user_assets_favorite", "Per-user favourited assets (admin).", "user_id", "user")
	dUserArchived = groupUsers.desc("user_assets_archived", "Per-user archived assets (admin).", "user_id", "user")
	dUserTrashed  = groupUsers.desc("user_assets_trashed", "Per-user assets in the trash (admin).", "user_id", "user")
	dUserLocked   = groupUsers.desc("user_assets_locked", "Per-user assets in the PIN-locked folder (admin).", "user_id", "user")
)

func (p *poller) collectUsers(ctx context.Context, s *snapshot) error {
	var users []struct {
		Status  string `json:"status"`
		IsAdmin bool   `json:"isAdmin"`
	}
	if err := p.c.Get(ctx, "/admin/users?withDeleted=true", &users); err != nil {
		return err
	}
	s.users.ok = true
	for _, u := range users {
		st := u.Status
		if st == "" {
			st = "active"
		}
		s.users.byStatus[st]++
		if u.IsAdmin {
			s.users.byRole["admin"]++
		} else {
			s.users.byRole["user"]++
		}
	}
	return nil
}

// userAssetStates are the admin statistics queries behind each field of userAssets.
var userAssetStates = []struct {
	query string
	field func(*userAssets) *float64
}{
	{"isFavorite=true", func(u *userAssets) *float64 { return &u.favorite }},
	{"visibility=archive", func(u *userAssets) *float64 { return &u.archived }},
	{"isTrashed=true", func(u *userAssets) *float64 { return &u.trashed }},
	// Search refuses the locked folder to an API key ("Elevated permission is required"); these statistics don't.
	{"visibility=locked", func(u *userAssets) *float64 { return &u.locked }},
}

// collectUserAssets fails as a whole when one user's statistics do: the locked total must not pass for every user's.
func (p *poller) collectUserAssets(ctx context.Context, s *snapshot) error {
	type user struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var users []user
	if err := p.c.Get(ctx, "/admin/users", &users); err != nil {
		return err
	}
	if len(users) > p.cfg.FanoutLimit {
		slog.Warn("per-user asset states skipped: too many users", "users", len(users), "limit", p.cfg.FanoutLimit)
		return nil
	}
	var failed atomic.Pointer[error]
	assets := parallel(users, p.cfg.FanoutConcurrency, func(u user) (userAssets, bool) {
		ua := userAssets{id: u.ID, name: u.Name}
		for _, st := range userAssetStates {
			var res struct {
				Total float64 `json:"total"`
			}
			if err := p.c.Get(ctx, "/admin/users/"+u.ID+"/statistics?"+st.query, &res); err != nil {
				failed.Store(&err)
				return ua, false
			}
			*st.field(&ua) = res.Total
		}
		return ua, true
	})
	if err := failed.Load(); err != nil {
		return *err
	}
	var locked float64
	for _, ua := range assets {
		locked += ua.locked
	}
	s.users.assets, s.assets.locked = assets, set(locked)
	return nil
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
		if us.photoBytes.ok {
			emit(ch, dUserStorage, us.photoBytes.v, us.id, us.name, "IMAGE")
		}
		if us.videoBytes.ok {
			emit(ch, dUserStorage, us.videoBytes.v, us.id, us.name, "VIDEO")
		}
		if us.quotaUnlimited {
			emit(ch, dUserUnlim, 1, us.id, us.name)
		} else {
			emit(ch, dUserUnlim, 0, us.id, us.name)
			emit(ch, dUserQuota, us.quotaBytes, us.id, us.name)
		}
	}
	for _, ua := range u.assets {
		emit(ch, dUserFavorite, ua.favorite, ua.id, ua.name)
		emit(ch, dUserArchived, ua.archived, ua.id, ua.name)
		emit(ch, dUserTrashed, ua.trashed, ua.id, ua.name)
		emit(ch, dUserLocked, ua.locked, ua.id, ua.name)
	}
}
