package exporter

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
)

type camerasSnap struct {
	makes, models, lenses   opt
	byMake, byModel, byLens map[string]float64
}

var (
	// cameras / EXIF
	dCameraMakes   = desc("camera_makes", "Number of distinct camera makes in the library.")
	dCameraModels  = desc("camera_models", "Number of distinct camera models in the library.")
	dLenses        = desc("lenses", "Number of distinct lens models in the library.")
	dAssetsByMake  = desc("assets_by_camera_make", "Assets per camera make.", "make")
	dAssetsByModel = desc("assets_by_camera_model", "Assets per camera model.", "model")
	dAssetsByLens  = desc("assets_by_lens", "Assets per lens model.", "lens")
)

func (p *poller) collectCameras(ctx context.Context, s *snapshot) error {
	makes, err := p.c.Suggest(ctx, "camera-make")
	if err != nil {
		return err
	}
	s.cameras.makes = set(float64(len(makes)))
	models, err := p.c.Suggest(ctx, "camera-model")
	if err != nil {
		return err
	}
	s.cameras.models = set(float64(len(models)))
	lenses, err := p.c.Suggest(ctx, "camera-lens-model")
	if err != nil {
		return err
	}
	s.cameras.lenses = set(float64(len(lenses)))

	s.cameras.byMake = p.fanout(ctx, makes, "make", 0) // makes are low-cardinality: emit all
	s.cameras.byModel = p.fanout(ctx, models, "model", p.cfg.TopN)
	s.cameras.byLens = p.fanout(ctx, lenses, "lensModel", p.cfg.TopN)
	return nil
}

// fanout counts assets per distinct value via search/statistics, with bounded
// concurrency. If the value list exceeds fanoutLimit it is skipped (returns nil)
// to bound API load. When n>0 only the top-n by count are kept, the rest folded
// into an "other" bucket.
func (p *poller) fanout(ctx context.Context, values []string, field string, n int) map[string]float64 {
	if len(values) == 0 {
		return nil
	}
	if len(values) > p.cfg.FanoutLimit {
		slog.Warn("fanout skipped: too many distinct values", "field", field, "values", len(values), "limit", p.cfg.FanoutLimit)
		return nil
	}
	type kv struct {
		k string
		v float64
	}
	counts := parallel(values, p.cfg.FanoutConcurrency, func(v string) (kv, bool) {
		c, err := p.count(ctx, where(field, v))
		if err != nil {
			slog.Warn("fanout call failed", "field", field, "value", v, "err", err)
			return kv{}, false
		}
		return kv{v, c}, true
	})
	m := make(map[string]float64, len(counts))
	for _, e := range counts {
		m[e.k] = e.v
	}
	return topNWithOther(m, n)
}

func emitCameras(ch chan<- prometheus.Metric, s *snapshot) {
	c := &s.cameras
	emitOpt(ch, dCameraMakes, c.makes)
	emitOpt(ch, dCameraModels, c.models)
	emitOpt(ch, dLenses, c.lenses)
	for k, v := range c.byMake {
		emit(ch, dAssetsByMake, v, k)
	}
	for k, v := range c.byModel {
		emit(ch, dAssetsByModel, v, k)
	}
	for k, v := range c.byLens {
		emit(ch, dAssetsByLens, v, k)
	}
}
