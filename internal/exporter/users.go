package exporter

import (
	"context"

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

type usersSnap struct {
	byStatus, byRole map[string]float64
	ok               bool
	perUser          []userStat
}

var (
	// users
	dUsers       = desc("users", "Number of users by status.", "status")
	dUsersByRole = desc("users_by_role", "Number of users by role.", "role")
	dUserPhotos  = desc("user_photos", "Per-user photo count.", "user_id", "user")
	dUserVideos  = desc("user_videos", "Per-user video count.", "user_id", "user")
	dUserUsage   = desc("user_usage_bytes", "Per-user total storage usage, in bytes.", "user_id", "user")
	dUserStorage = desc("user_storage_bytes", "Per-user storage usage by asset type, in bytes.", "user_id", "user", "type")
	dUserQuota   = desc("user_quota_bytes", "Per-user storage quota in bytes (omitted when unlimited).", "user_id", "user")
	dUserUnlim   = desc("user_quota_unlimited", "1 if the user has no storage quota (unlimited).", "user_id", "user")
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

// collectLocked sums every user's locked-folder assets. Search refuses the
// locked visibility to an API key ("Elevated permission is required"), but the
// admin per-user statistics answer it.
func (p *poller) collectLocked(ctx context.Context, s *snapshot) error {
	var users []struct {
		ID string `json:"id"`
	}
	if err := p.c.Get(ctx, "/admin/users", &users); err != nil {
		return err
	}
	var locked float64
	for _, u := range users {
		var st struct {
			Total float64 `json:"total"`
		}
		if err := p.c.Get(ctx, "/admin/users/"+u.ID+"/statistics?visibility=locked", &st); err != nil {
			return err
		}
		locked += st.Total
	}
	s.assets.locked = set(locked)
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
}
