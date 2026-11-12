package exporter

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/moond4rk/immich-exporter/internal/immich"
)

// jobQueueStat is one (queue,state) -> count datapoint plus the paused flag.
type jobQueueStat struct {
	queue, state string
	count        float64
}

type jobsSnap struct {
	queues []jobQueueStat
	paused map[string]float64
}

var (
	// jobs
	dJobQueue       = desc("job_queue", "Jobs in each queue by state.", "queue", "state")
	dJobQueuePaused = desc("job_queue_paused", "1 if a job queue is paused.", "queue")
)

// queueCounts is the per-state depth shared by /queues (v2.4.0+) and the legacy
// /jobs map (v1–v3, deprecated at v2.4.0).
type queueCounts struct {
	Active    float64 `json:"active"`
	Completed float64 `json:"completed"`
	Failed    float64 `json:"failed"`
	Delayed   float64 `json:"delayed"`
	Waiting   float64 `json:"waiting"`
	Paused    float64 `json:"paused"`
}

// collectJobs prefers /queues (added v2.4.0); it falls back to the deprecated
// /jobs map on 403/404 — older servers lack /queues, and keys scoped to job.read
// (not queue.read) get 403 — so both metric shapes keep the same series.
func (p *poller) collectJobs(ctx context.Context, s *snapshot) error {
	var queues []struct {
		Name       string      `json:"name"`
		IsPaused   bool        `json:"isPaused"`
		Statistics queueCounts `json:"statistics"`
	}
	err := p.c.Get(ctx, "/queues", &queues)
	if immich.StatusIs(err, 403, 404) {
		return p.collectJobsLegacy(ctx, s)
	}
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(queues))
	for _, q := range queues {
		if seen[q.Name] {
			continue // guard against duplicate names crashing the whole /metrics scrape
		}
		seen[q.Name] = true
		addQueue(s, q.Name, q.IsPaused, q.Statistics)
	}
	return nil
}

func (p *poller) collectJobsLegacy(ctx context.Context, s *snapshot) error {
	var jobs map[string]struct {
		JobCounts   queueCounts `json:"jobCounts"`
		QueueStatus struct {
			IsPaused bool `json:"isPaused"`
		} `json:"queueStatus"`
	}
	if err := p.c.Get(ctx, "/jobs", &jobs); err != nil {
		return err
	}
	for name, j := range jobs {
		addQueue(s, name, j.QueueStatus.IsPaused, j.JobCounts)
	}
	return nil
}

func addQueue(s *snapshot, name string, paused bool, c queueCounts) {
	s.jobs.queues = append(s.jobs.queues,
		jobQueueStat{name, "active", c.Active},
		jobQueueStat{name, "completed", c.Completed},
		jobQueueStat{name, "failed", c.Failed},
		jobQueueStat{name, "delayed", c.Delayed},
		jobQueueStat{name, "waiting", c.Waiting},
		jobQueueStat{name, "paused", c.Paused},
	)
	s.jobs.paused[name] = boolf(paused)
}

func emitJobs(ch chan<- prometheus.Metric, s *snapshot) {
	for _, jq := range s.jobs.queues {
		emit(ch, dJobQueue, jq.count, jq.queue, jq.state)
	}
	for q, v := range s.jobs.paused {
		emit(ch, dJobQueuePaused, v, q)
	}
}
