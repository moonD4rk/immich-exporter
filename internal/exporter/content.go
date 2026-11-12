package exporter

import (
	"context"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/moond4rk/immich-exporter/internal/immich"
)

type contentSnap struct {
	tags, tagsRoot                 float64
	tagsOK                         bool
	memories                       opt
	duplicateSets, duplicateAssets float64
	duplicatesOK                   bool
	stacks, stackedAssets          float64
	stacksOK                       bool
	libraries                      opt
	perLibrary                     []labeledVal
	perLibraryBytes                []labeledVal
	apiKeys, sessions              opt
	notifUnread                    float64
	notifOK                        bool
	notifByLevel                   map[string]float64
	workflows                      map[string]float64 // enabled "true"/"false" -> count
	plugins                        opt
}

var (
	// content extras
	dTags          = desc("tags", "Number of tags.")
	dTagsRoot      = desc("tags_root", "Number of top-level (root) tags.")
	dMemories      = desc("memories", "Number of memories.")
	dDupSets       = desc("duplicate_sets", "Number of duplicate clusters detected.")
	dDupAssets     = desc("duplicate_assets", "Number of assets involved in duplicate sets.")
	dStacks        = desc("stacks", "Number of asset stacks.")
	dStackedAssets = desc("stacked_assets", "Number of assets grouped into stacks.")
	dLibraries     = desc("libraries", "Number of configured external libraries.")
	dLibAssets     = desc("library_assets", "Assets per external library.", "library_id", "library")
	dLibUsage      = desc("library_usage_bytes", "Storage used per external library, in bytes.", "library_id", "library")
	dAPIKeys       = desc("api_keys", "Number of API keys for the key owner.")
	dSessions      = desc("sessions", "Number of active sessions for the key owner.")
	dNotifUnread   = desc("notifications_unread", "Number of unread notifications.")
	dNotifByLevel  = desc("notifications_by_level", "Notifications by level.", "level")
	dWorkflows     = desc("workflows", "Workflows by whether they are enabled (Immich 3+).", "enabled")
	dPlugins       = desc("plugins", "Installed plugins (Immich 3+).")
)

func (p *poller) collectTags(ctx context.Context, s *snapshot) error {
	var tags []struct {
		ParentID *string `json:"parentId"`
	}
	if err := p.c.Get(ctx, "/tags", &tags); err != nil {
		return err
	}
	s.content.tagsOK = true
	s.content.tags = float64(len(tags))
	for _, t := range tags {
		if t.ParentID == nil {
			s.content.tagsRoot++
		}
	}
	return nil
}

func (p *poller) collectMemories(ctx context.Context, s *snapshot) error {
	var st struct {
		Total float64 `json:"total"`
	}
	if err := p.c.Get(ctx, "/memories/statistics", &st); err != nil {
		return err
	}
	s.content.memories = set(st.Total)
	return nil
}

func (p *poller) collectLibraries(ctx context.Context, s *snapshot) error {
	var libs []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := p.c.Get(ctx, "/libraries", &libs); err != nil {
		return err
	}
	s.content.libraries = set(float64(len(libs)))
	for _, l := range libs {
		// The list's assetCount is always 0 on v3; only /statistics is authoritative.
		var st struct {
			Total float64  `json:"total"`
			Usage *float64 `json:"usage"`
		}
		if err := p.c.Get(ctx, "/libraries/"+l.ID+"/statistics", &st); err != nil {
			if isSoftStatus(err) {
				continue // key lacks library.statistics: leave the sample absent, never a false zero
			}
			return err
		}
		s.content.perLibrary = append(s.content.perLibrary, labeledVal{id: l.ID, name: l.Name, value: st.Total})
		if st.Usage != nil {
			s.content.perLibraryBytes = append(s.content.perLibraryBytes, labeledVal{id: l.ID, name: l.Name, value: *st.Usage})
		}
	}
	return nil
}

func (p *poller) collectAPIKeys(ctx context.Context, s *snapshot) error {
	var keys []struct{}
	if err := p.c.Get(ctx, "/api-keys", &keys); err != nil {
		return err
	}
	s.content.apiKeys = set(float64(len(keys)))
	return nil
}

func (p *poller) collectSessions(ctx context.Context, s *snapshot) error {
	var sess []struct{}
	if err := p.c.Get(ctx, "/sessions", &sess); err != nil {
		return err
	}
	s.content.sessions = set(float64(len(sess)))
	return nil
}

func (p *poller) collectNotifications(ctx context.Context, s *snapshot) error {
	var notifs []struct {
		Level  string  `json:"level"`
		ReadAt *string `json:"readAt"`
	}
	if err := p.c.Get(ctx, "/notifications", &notifs); err != nil {
		return err
	}
	s.content.notifOK = true
	for _, n := range notifs {
		if n.ReadAt == nil {
			s.content.notifUnread++
		}
		if n.Level != "" {
			s.content.notifByLevel[n.Level]++
		}
	}
	return nil
}

// collectWorkflows counts the key owner's workflows (Immich 3+). Both states are
// always reported so a library without workflows shows 0 rather than no data.
func (p *poller) collectWorkflows(ctx context.Context, s *snapshot) error {
	var workflows []struct {
		Enabled bool `json:"enabled"`
	}
	if err := p.c.Get(ctx, "/workflows", &workflows); err != nil {
		return err
	}
	w := s.content.workflows
	w["true"], w["false"] = 0, 0
	for _, wf := range workflows {
		w[strconv.FormatBool(wf.Enabled)]++
	}
	return nil
}

func (p *poller) collectPlugins(ctx context.Context, s *snapshot) error {
	var plugins []struct{}
	if err := p.c.Get(ctx, "/plugins", &plugins); err != nil {
		return err
	}
	s.content.plugins = set(float64(len(plugins)))
	return nil
}

type assetGroup struct {
	Assets []struct{} `json:"assets"`
}

// countGroups counts the groups a whole-library listing returns and the assets in them, without holding the listing.
func (p *poller) countGroups(ctx context.Context, path string) (groups, assets float64, err error) {
	err = immich.Each(ctx, p.c, path, func(g assetGroup) {
		groups++
		assets += float64(len(g.Assets))
	})
	return groups, assets, err
}

func (p *poller) collectDuplicates(ctx context.Context, s *snapshot) error {
	sets, assets, err := p.countGroups(ctx, "/duplicates")
	if err != nil {
		return err
	}
	s.content.duplicateSets, s.content.duplicateAssets, s.content.duplicatesOK = sets, assets, true
	return nil
}

func (p *poller) collectStacks(ctx context.Context, s *snapshot) error {
	stacks, assets, err := p.countGroups(ctx, "/stacks")
	if err != nil {
		return err
	}
	s.content.stacks, s.content.stackedAssets, s.content.stacksOK = stacks, assets, true
	return nil
}

func emitContent(ch chan<- prometheus.Metric, s *snapshot) {
	c := &s.content
	if c.tagsOK {
		emit(ch, dTags, c.tags)
		emit(ch, dTagsRoot, c.tagsRoot)
	}
	emitOpt(ch, dMemories, c.memories)
	if c.duplicatesOK {
		emit(ch, dDupSets, c.duplicateSets)
		emit(ch, dDupAssets, c.duplicateAssets)
	}
	if c.stacksOK {
		emit(ch, dStacks, c.stacks)
		emit(ch, dStackedAssets, c.stackedAssets)
	}
	emitOpt(ch, dLibraries, c.libraries)
	for _, l := range c.perLibrary {
		emit(ch, dLibAssets, l.value, l.id, l.name)
	}
	for _, l := range c.perLibraryBytes {
		emit(ch, dLibUsage, l.value, l.id, l.name)
	}
	emitOpt(ch, dAPIKeys, c.apiKeys)
	emitOpt(ch, dSessions, c.sessions)
	if c.notifOK {
		emit(ch, dNotifUnread, c.notifUnread)
		for lvl, v := range c.notifByLevel {
			emit(ch, dNotifByLevel, v, lvl)
		}
	}
	for enabled, v := range c.workflows {
		emit(ch, dWorkflows, v, enabled)
	}
	emitOpt(ch, dPlugins, c.plugins)
}
