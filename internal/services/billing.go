// This file holds the traffic accounting of the sync engine: what a run moves
// through each of its accounts, in which direction, per UTC day and month. The
// rows feed the billing report (see docs/architecture.md and the billing
// endpoints in docs/api.md); the engine calls into this file from the three
// places that touch a provider (copy, remove, ensureFolder) plus once at the end
// of a run.
package services

import (
	"context"
	"log/slog"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// trafficProgressEvery throttles the writes of the traffic counters, so a run
// over thousands of small files is a handful of upserts instead of one per file.
const trafficProgressEvery = 5 * time.Second

// trafficKey identifies one counter of a run: an account and a direction. A job
// whose two endpoints are the same account (Google → Google) therefore keeps two
// rows, which is what makes the inbound/outbound split meaningful.
type trafficKey struct {
	account   uint
	direction models.TrafficDirection
}

// trafficCounter is the running total of one (account, direction) of a run.
type trafficCounter struct {
	email      string
	provider   string
	bytes      int64
	files      int
	operations int
}

// trafficDirection is the side of the job an endpoint stands for: the source is
// read from (inbound) and the destination is written to (outbound).
func (e *engine) trafficDirection(side *endpoint) models.TrafficDirection {
	if side == e.source {
		return models.TrafficInbound
	}
	return models.TrafficOutbound
}

// addTraffic records the payload of one transfer and the payload-free write calls
// (a delete, a folder creation) against an account. Bytes, files and operations
// are the absolute counters of the run: flushing them twice is harmless.
func (e *engine) addTraffic(account *models.ConnectedAccount, direction models.TrafficDirection, bytes int64, files, operations int) {
	if account == nil || (bytes <= 0 && files <= 0 && operations <= 0) {
		return
	}
	if e.traffic == nil {
		e.traffic = map[trafficKey]trafficCounter{}
	}
	key := trafficKey{account: account.ID, direction: direction}
	counter := e.traffic[key]
	counter.email = account.Email
	counter.provider = string(account.Provider)
	counter.bytes += bytes
	counter.files += files
	counter.operations += operations
	e.traffic[key] = counter
}

// flushTraffic stores the counters as rows keyed by (run, account, direction).
//
// While the run is in flight the write is throttled and fired from the transfer
// path; `force` is used once at the end, so a finished run is exact. A cancel or
// a crash keeps whatever the last flush stored, which is the traffic that really
// moved.
func (e *engine) flushTraffic(ctx context.Context, force bool) {
	if len(e.traffic) == 0 || e.run == nil || e.run.ID == 0 {
		return
	}
	now := e.service.now()
	if !force && now.Sub(e.lastTrafficFlush) < trafficProgressEvery {
		return
	}
	e.lastTrafficFlush = now

	started := e.run.StartedAt.UTC()
	if started.IsZero() {
		started = now
	}
	rows := make([]models.TrafficEntry, 0, len(e.traffic))
	for key, counter := range e.traffic {
		rows = append(rows, models.TrafficEntry{
			RunID:        e.run.ID,
			JobID:        e.job.ID,
			AccountID:    key.account,
			AccountEmail: counter.email,
			Provider:     counter.provider,
			Direction:    string(key.direction),
			Bytes:        counter.bytes,
			Files:        counter.files,
			Operations:   counter.operations,
			Day:          started.Format(models.TrafficDayFormat),
			Month:        started.Format(models.TrafficMonthFormat),
			StartedAt:    started,
			CreatedAt:    now,
			UpdatedAt:    now,
		})
	}
	if err := e.service.store.SaveTraffic(ctx, rows); err != nil {
		slog.Warn("storing the run traffic failed", "job", e.job.ID, "run", e.run.ID, "error", err)
	}
}
