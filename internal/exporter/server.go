package exporter

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/moond4rk/immich-exporter/internal/immich"
)

// aboutInfo mirrors the /server/about fields exported as immich_server_info
// labels; a typed struct keeps the poller and the label order of dServerInfo
// aligned at compile time.
type aboutInfo struct {
	version, sourceRef, sourceCommit, build,
	nodejs, exiftool, ffmpeg, imagemagick, libvips string
	ok bool
}

type serverSnap struct {
	about                                    aboutInfo
	licensed                                 opt
	latestVersion                            string // empty while the upstream version check is stale
	updateAvail                              float64
	maintenance                              opt
	versionInstalledUnix, versionCheckedUnix opt
	features                                 map[string]float64 // feature -> 0/1; keys change across Immich versions, so a map is deliberate
	trashDays, userDeleteDelayDays, minFaces,
	initialized, onboarded opt
	// storage of the volume backing UPLOAD_LOCATION; the three are set together
	storageSizeBytes, storageUsedBytes, storageAvailableBytes opt
}

var (
	// server / system
	dServerUp         = groupServer.desc("up", "1 if the Immich server answered /server/ping.")
	dServerInfo       = groupServer.desc("server_info", "Immich server version and dependency metadata (value is always 1).", "version", "source_ref", "source_commit", "build", "nodejs", "exiftool", "ffmpeg", "imagemagick", "libvips")
	dServerLicensed   = groupServer.desc("server_licensed", "1 if the Immich server has an active license.")
	dLatestVersion    = groupServer.desc("server_latest_version_info", "Latest Immich release available upstream (value is always 1). Absent while Immich's version check is stale.", "release_version")
	dUpdateAvailable  = groupServer.desc("server_update_available", "1 if a newer Immich release is available upstream. Absent while Immich's version check is stale.")
	dVersionChecked   = groupServer.desc("server_version_check_timestamp_seconds", "Unix time of Immich's latest upstream version check; older than a week counts as stale.")
	dVersionInstalled = groupServer.desc("server_version_installed_timestamp_seconds", "Unix time the running Immich version first started, from the server's version history.")
	dMaintenance      = groupServer.desc("server_maintenance_mode", "1 while Immich is in maintenance mode; the exporter then collects nothing else.")
	dFeature          = groupServer.desc("server_feature", "Immich server feature flags (1 enabled, 0 disabled).", "feature")
	dConfigTrashDays  = groupServer.desc("config_trash_days", "Days assets stay in trash before automatic deletion.")
	dConfigDelDelay   = groupServer.desc("config_user_delete_delay_days", "Grace period before a deleted user is purged, in days.")
	dConfigMinFaces   = groupServer.desc("config_min_faces", "Minimum faces threshold for facial-recognition clustering.")
	dInitialized      = groupServer.desc("server_initialized", "1 if the server has completed first-run initialization.")
	dOnboarded        = groupServer.desc("server_onboarded", "1 if admin onboarding has completed.")

	// storage
	dStorageSize  = groupServer.desc("storage_size_bytes", "Total capacity of the filesystem backing the upload location.")
	dStorageUsed  = groupServer.desc("storage_used_bytes", "Used bytes on the upload filesystem (whole volume, not just Immich).")
	dStorageAvail = groupServer.desc("storage_available_bytes", "Free bytes on the upload filesystem.")
)

func (p *poller) ping(ctx context.Context) error {
	var out struct {
		Res string `json:"res"`
	}
	if err := p.c.Get(ctx, "/server/ping", &out); err != nil {
		return err
	}
	if out.Res != "pong" {
		return fmt.Errorf("GET /server/ping: answered %q, not pong", out.Res)
	}
	return nil
}

// collectMaintenance reads maintenance mode (Immich 2.5+), which needs no permission.
func (p *poller) collectMaintenance(ctx context.Context, s *snapshot) error {
	var st struct {
		Active bool `json:"active"`
	}
	if err := p.c.Get(ctx, "/admin/maintenance/status", &st); err != nil {
		return err
	}
	s.server.maintenance = set(boolf(st.Active))
	return nil
}

func (p *poller) collectMe(ctx context.Context, s *snapshot) error {
	var me struct {
		IsAdmin bool `json:"isAdmin"`
	}
	if err := p.c.Get(ctx, "/users/me", &me); err != nil {
		return err
	}
	s.health.keyIsAdmin = set(boolf(me.IsAdmin))
	return nil
}

func (p *poller) collectAbout(ctx context.Context, s *snapshot) error {
	var a struct {
		Version      string `json:"version"`
		SourceRef    string `json:"sourceRef"`
		SourceCommit string `json:"sourceCommit"`
		Build        string `json:"build"`
		Nodejs       string `json:"nodejs"`
		Exiftool     string `json:"exiftool"`
		Ffmpeg       string `json:"ffmpeg"`
		Imagemagick  string `json:"imagemagick"`
		Libvips      string `json:"libvips"`
		Licensed     bool   `json:"licensed"`
	}
	if err := p.c.Get(ctx, "/server/about", &a); err != nil {
		return err
	}
	s.server.about = aboutInfo{
		version: a.Version, sourceRef: a.SourceRef, sourceCommit: a.SourceCommit, build: a.Build,
		nodejs: a.Nodejs, exiftool: a.Exiftool, ffmpeg: a.Ffmpeg, imagemagick: a.Imagemagick,
		libvips: a.Libvips, ok: true,
	}
	s.server.licensed = set(boolf(a.Licensed))
	return nil
}

// versionCheckMaxAge bounds how old Immich's last upstream check may be before
// its answer is withheld. Disabling the check freezes the stored answer, which
// kept reporting v2.5.6 for months to a server long since on v3.
const versionCheckMaxAge = 7 * 24 * time.Hour

func (p *poller) collectVersionCheck(ctx context.Context, s *snapshot) error {
	var vc struct {
		ReleaseVersion string  `json:"releaseVersion"`
		CheckedAt      *string `json:"checkedAt"`
	}
	if err := p.c.Get(ctx, "/server/version-check", &vc); err != nil {
		return err
	}
	if vc.CheckedAt != nil {
		if t, err := time.Parse(time.RFC3339, *vc.CheckedAt); err == nil {
			s.server.versionCheckedUnix = set(float64(t.Unix()))
			if time.Since(t) > versionCheckMaxAge {
				return nil
			}
		}
	}
	if vc.ReleaseVersion == "" {
		return nil
	}
	release, relPre, relOK := parseSemver(vc.ReleaseVersion)
	running, runPre, runOK := parseSemver(s.server.about.version)
	if relOK && runOK && semverGreater(running, release) {
		return nil // a release older than the server: the check predates the last upgrade
	}
	s.server.latestVersion = vc.ReleaseVersion
	// A stable release also supersedes a running pre-release of the same core version.
	if relOK && runOK && (semverGreater(release, running) || (release == running && runPre && !relPre)) {
		s.server.updateAvail = 1
	}
	return nil
}

// collectVersionHistory finds when the running version first started, which
// needs no permission.
func (p *poller) collectVersionHistory(ctx context.Context, s *snapshot) error {
	var history []struct {
		Version   string `json:"version"`
		CreatedAt string `json:"createdAt"`
	}
	if err := p.c.Get(ctx, "/server/version-history", &history); err != nil {
		return err
	}
	running := strings.TrimPrefix(s.server.about.version, "v")
	var since time.Time
	for _, h := range history {
		t, err := time.Parse(time.RFC3339, h.CreatedAt)
		// The latest entry wins: a version reinstalled after a downgrade has been running since then.
		if err == nil && running != "" && strings.TrimPrefix(h.Version, "v") == running && t.After(since) {
			since = t
		}
	}
	if !since.IsZero() {
		s.server.versionInstalledUnix = set(float64(since.Unix()))
	}
	return nil
}

// collectFeatures reads /server/features, deprecated since Immich 3.2, and
// falls back to /config once a server drops it.
func (p *poller) collectFeatures(ctx context.Context, s *snapshot) error {
	var f map[string]bool
	err := p.c.Get(ctx, "/server/features", &f)
	if immich.StatusIs(err, 404) {
		return p.collectFeaturesFromConfig(ctx, s)
	}
	if err != nil {
		return err
	}
	for k, v := range f {
		s.server.features[camelToSnake(k)] = boolf(v)
	}
	return nil
}

type toggle struct {
	Enabled bool `json:"enabled"`
}

// userConfig is the part of GET /config (Immich 3.2+) that replaces /server/features and /server/config.
type userConfig struct {
	FFmpeg struct {
		Realtime toggle `json:"realtime"`
	} `json:"ffmpeg"`
	MachineLearning struct {
		Enabled            bool   `json:"enabled"`
		Clip               toggle `json:"clip"`
		DuplicateDetection toggle `json:"duplicateDetection"`
		Ocr                toggle `json:"ocr"`
		FacialRecognition  struct {
			Enabled  bool     `json:"enabled"`
			MinFaces *float64 `json:"minFaces"`
		} `json:"facialRecognition"`
	} `json:"machineLearning"`
	Map              toggle `json:"map"`
	ReverseGeocoding toggle `json:"reverseGeocoding"`
	OAuth            struct {
		Enabled    bool `json:"enabled"`
		AutoLaunch bool `json:"autoLaunch"`
	} `json:"oauth"`
	PasswordLogin toggle `json:"passwordLogin"`
	Trash         struct {
		Enabled bool     `json:"enabled"`
		Days    *float64 `json:"days"`
	} `json:"trash"`
	User struct {
		DeleteDelay *float64 `json:"deleteDelay"`
	} `json:"user"`
}

// collectFeaturesFromConfig derives the feature flags Immich computes for
// /server/features. /config has no counterpart for config_file, email,
// import_faces, search or sidecar, so those go absent.
func (p *poller) collectFeaturesFromConfig(ctx context.Context, s *snapshot) error {
	var c userConfig
	if err := p.c.Get(ctx, "/config", &c); err != nil {
		return err
	}
	ml := c.MachineLearning
	for name, on := range map[string]bool{
		"smart_search":         ml.Enabled && ml.Clip.Enabled,
		"facial_recognition":   ml.Enabled && ml.FacialRecognition.Enabled,
		"duplicate_detection":  ml.Enabled && ml.DuplicateDetection.Enabled,
		"ocr":                  ml.Enabled && ml.Ocr.Enabled,
		"map":                  c.Map.Enabled,
		"reverse_geocoding":    c.ReverseGeocoding.Enabled,
		"oauth":                c.OAuth.Enabled,
		"oauth_auto_launch":    c.OAuth.AutoLaunch,
		"password_login":       c.PasswordLogin.Enabled,
		"realtime_transcoding": c.FFmpeg.Realtime.Enabled,
		"trash":                c.Trash.Enabled,
	} {
		s.server.features[name] = boolf(on)
	}
	return nil
}

// collectConfig reads /server/config, deprecated since Immich 3.2, and falls
// back to /config once a server drops it; /config has no initialized or
// onboarded flag, so those go absent then.
func (p *poller) collectConfig(ctx context.Context, s *snapshot) error {
	var c struct {
		TrashDays       *float64 `json:"trashDays"`
		UserDeleteDelay *float64 `json:"userDeleteDelay"`
		MinFaces        *float64 `json:"minFaces"`
		IsInitialized   *bool    `json:"isInitialized"`
		IsOnboarded     *bool    `json:"isOnboarded"`
	}
	err := p.c.Get(ctx, "/server/config", &c)
	switch {
	case immich.StatusIs(err, 404):
		return p.collectConfigFromUserConfig(ctx, s)
	case err != nil:
		return err
	}
	sv := &s.server
	sv.trashDays, sv.userDeleteDelayDays, sv.minFaces = optional(c.TrashDays), optional(c.UserDeleteDelay), optional(c.MinFaces)
	if c.IsInitialized != nil {
		sv.initialized = set(boolf(*c.IsInitialized))
	}
	if c.IsOnboarded != nil {
		sv.onboarded = set(boolf(*c.IsOnboarded))
	}
	return nil
}

func (p *poller) collectConfigFromUserConfig(ctx context.Context, s *snapshot) error {
	var c userConfig
	if err := p.c.Get(ctx, "/config", &c); err != nil {
		return err
	}
	sv := &s.server
	sv.trashDays, sv.userDeleteDelayDays = optional(c.Trash.Days), optional(c.User.DeleteDelay)
	sv.minFaces = optional(c.MachineLearning.FacialRecognition.MinFaces)
	return nil
}

func (p *poller) collectStorage(ctx context.Context, s *snapshot) error {
	var st struct {
		DiskSizeRaw      float64 `json:"diskSizeRaw"`
		DiskUseRaw       float64 `json:"diskUseRaw"`
		DiskAvailableRaw float64 `json:"diskAvailableRaw"`
	}
	if err := p.c.Get(ctx, "/server/storage", &st); err != nil {
		return err
	}
	s.server.storageSizeBytes = set(st.DiskSizeRaw)
	s.server.storageUsedBytes = set(st.DiskUseRaw)
	s.server.storageAvailableBytes = set(st.DiskAvailableRaw)
	return nil
}

// parseSemver reads "v1.2.3[-pre][+build]" into numeric core fields plus a
// pre-release flag (build metadata never affects precedence). ok=false means
// the comparison is unknowable, and callers must not report an update rather
// than risk a false positive.
func parseSemver(v string) (parts [3]int, pre, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "+")
	v, _, pre = strings.Cut(v, "-")
	fields := strings.Split(v, ".")
	if len(fields) == 0 || len(fields) > 3 || fields[0] == "" {
		return parts, pre, false
	}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return parts, pre, false
		}
		parts[i] = n
	}
	return parts, pre, true
}

func semverGreater(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func emitServer(ch chan<- prometheus.Metric, s *snapshot) {
	sv := &s.server
	emit(ch, dServerUp, s.health.serverUp)
	if a := sv.about; a.ok {
		emit(ch, dServerInfo, 1, a.version, a.sourceRef, a.sourceCommit, a.build,
			a.nodejs, a.exiftool, a.ffmpeg, a.imagemagick, a.libvips)
	}
	emitOpt(ch, dServerLicensed, sv.licensed)
	if sv.latestVersion != "" {
		emit(ch, dLatestVersion, 1, sv.latestVersion)
		emit(ch, dUpdateAvailable, sv.updateAvail)
	}
	emitOpt(ch, dVersionChecked, sv.versionCheckedUnix)
	emitOpt(ch, dVersionInstalled, sv.versionInstalledUnix)
	emitOpt(ch, dMaintenance, sv.maintenance)
	for f, v := range sv.features {
		emit(ch, dFeature, v, f)
	}
	emitOpt(ch, dConfigTrashDays, sv.trashDays)
	emitOpt(ch, dConfigDelDelay, sv.userDeleteDelayDays)
	emitOpt(ch, dConfigMinFaces, sv.minFaces)
	emitOpt(ch, dInitialized, sv.initialized)
	emitOpt(ch, dOnboarded, sv.onboarded)
	emitOpt(ch, dStorageSize, sv.storageSizeBytes)
	emitOpt(ch, dStorageUsed, sv.storageUsedBytes)
	emitOpt(ch, dStorageAvail, sv.storageAvailableBytes)
}
