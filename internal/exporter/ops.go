package exporter

import (
	"context"
	"maps"
	"regexp"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// opsSnap is Immich's own upkeep: its integrity checks and database backups.
type opsSnap struct {
	integrity                       map[string]float64 // report type -> flagged files
	backups, backupsBytes           float64
	backupsOK                       bool
	lastBackupUnix, lastBackupBytes opt
}

var (
	// integrity checks & database backups (admin)
	dIntegrity      = desc("integrity_issues", "Files flagged by Immich's latest integrity checks, by report type.", "type")
	dBackups        = desc("database_backups", "Database backups in Immich's backups folder.")
	dBackupsSize    = desc("database_backups_size_bytes", "Total size of the database backups in Immich's backups folder.")
	dBackupLast     = desc("database_backup_last_timestamp_seconds", "Unix time of the newest database backup, read from its file name.")
	dBackupLastSize = desc("database_backup_last_size_bytes", "Size of the newest database backup, in bytes.")
)

// collectIntegrity reads the files Immich's own integrity checks (Immich 3+) flagged on their latest run.
func (p *poller) collectIntegrity(ctx context.Context, s *snapshot) error {
	var summary map[string]float64
	if err := p.c.Get(ctx, "/admin/integrity/summary", &summary); err != nil {
		return err
	}
	maps.Copy(s.ops.integrity, summary)
	return nil
}

// backupStamp is the local time Immich writes into a backup's file name, e.g.
// immich-db-backup-20261022T020000-v3.3.3-pg18.6.sql.gz; an uploaded backup
// may carry a prefix before it.
var backupStamp = regexp.MustCompile(`immich-db-backup-(\d{8}T\d{6})-`)

// collectBackups lists the database backups Immich (2.5+) keeps.
func (p *poller) collectBackups(ctx context.Context, s *snapshot) error {
	var res struct {
		Backups []struct {
			Filename string  `json:"filename"`
			Filesize float64 `json:"filesize"`
			Timezone string  `json:"timezone"`
		} `json:"backups"`
	}
	if err := p.c.Get(ctx, "/admin/database-backups", &res); err != nil {
		return err
	}
	o := &s.ops
	o.backupsOK = true
	o.backups = float64(len(res.Backups))
	var newest time.Time
	for _, b := range res.Backups {
		o.backupsBytes += b.Filesize
		if t, ok := backupTime(b.Filename, b.Timezone); ok && t.After(newest) {
			newest = t
			o.lastBackupBytes = set(b.Filesize)
		}
	}
	if !newest.IsZero() {
		o.lastBackupUnix = set(float64(newest.Unix()))
	}
	return nil
}

// backupTime reads when Immich wrote a backup from its file name, stamped in
// the server's time zone; an unknown zone falls back to UTC.
func backupTime(name, zone string) (time.Time, bool) {
	m := backupStamp.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	t, err := time.ParseInLocation("20060102T150405", m[1], loc)
	return t, err == nil
}

func emitOps(ch chan<- prometheus.Metric, s *snapshot) {
	o := &s.ops
	for t, v := range o.integrity {
		emit(ch, dIntegrity, v, t)
	}
	if o.backupsOK {
		emit(ch, dBackups, o.backups)
		emit(ch, dBackupsSize, o.backupsBytes)
	}
	emitOpt(ch, dBackupLast, o.lastBackupUnix)
	emitOpt(ch, dBackupLastSize, o.lastBackupBytes)
}
