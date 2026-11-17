package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// DefaultReaperInterval is how often RunReaper looks for expired leases
// unless told otherwise.
const DefaultReaperInterval = 15 * time.Second

// ReapResult says what one pass of the reaper did.
type ReapResult struct {
	// Requeued holds the ids of the jobs queued again, in the order their
	// leases expired.
	Requeued []int64
	// Failed holds the ids of the jobs marked failed because they had used
	// up their attempts.
	Failed []int64
}

// Reap takes back every job whose lease has expired: it is queued again, or
// marked failed once it has been handed out MaxAttempts times, with a reason
// that names the clipper that went quiet. It makes one pass and returns;
// RunReaper calls it on a timer.
//
// The expired jobs are found on a read connection and each is taken back in
// a write transaction of its own, which reads the job again first: a job
// whose holder sent a heartbeat, failed it or had it taken back in the
// meantime is left alone. So the write connection is never held for more
// than one short transaction, and the job API keeps running between them.
//
// On an error Reap stops and returns what it did so far with the error. The
// jobs it did not reach are taken back by a later pass.
func (q *Queue) Reap(ctx context.Context) (ReapResult, error) {
	var res ReapResult
	ids, err := q.expiredJobs(ctx)
	if err != nil {
		return res, err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		job, reaped, err := q.reapJob(ctx, id)
		if err != nil {
			return res, err
		}
		switch {
		case !reaped:
		case job.Status == store.JobQueued:
			res.Requeued = append(res.Requeued, job.ID)
		default:
			res.Failed = append(res.Failed, job.ID)
		}
	}
	return res, nil
}

// expiredJobs returns the ids of the leased jobs whose lease has run out,
// earliest expiry first. jobs_status_lease serves the query.
func (q *Queue) expiredJobs(ctx context.Context) ([]int64, error) {
	rows, err := q.store.Reader().QueryContext(ctx, `
		SELECT id FROM jobs
		WHERE status = ? AND lease_expires_at <= ?
		ORDER BY lease_expires_at, id`,
		store.JobLeased, store.FormatTime(q.stamp()))
	if err != nil {
		return nil, fmt.Errorf("queue: find expired leases: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("queue: find expired leases: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: find expired leases: %w", err)
	}
	return ids, nil
}

// reapJob takes one job back if its lease is still expired. reaped is false
// when the job changed since it was found and was left alone.
func (q *Queue) reapJob(ctx context.Context, id int64) (job store.Job, reaped bool, err error) {
	err = q.store.WriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := getJob(ctx, tx, id)
		if errors.Is(err, ErrUnknownJob) {
			return nil
		}
		if err != nil {
			return err
		}
		if cur.Status != store.JobLeased || cur.LeaseExpiresAt.After(q.stamp()) {
			return nil
		}
		reason := fmt.Sprintf("lease expired: no heartbeat from clipper %q since %s",
			cur.Clipper, store.FormatTime(cur.LastHeartbeatAt))
		if job, err = q.release(ctx, tx, cur, reason, true); err != nil {
			return err
		}
		reaped = true
		return nil
	})
	if err != nil {
		return store.Job{}, false, fmt.Errorf("queue: reap job %d: %w", id, err)
	}
	return job, reaped, nil
}

// RunReaper takes back expired leases until ctx is cancelled: it calls Reap
// once at the start, so leases that expired while pogfarm was down come back
// at once, and then every interval. It returns nil once ctx is cancelled,
// promptly, and an error only when interval is not positive.
//
// A pass that fails is logged to logger and retried at the next tick; logger
// may be nil to discard the log. Run it in a goroutine of its own, beside the
// job API.
func (q *Queue) RunReaper(ctx context.Context, interval time.Duration, logger *slog.Logger) error {
	if interval <= 0 {
		return fmt.Errorf("queue: reaper interval %s is not positive", interval)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	q.runReaper(ctx, ticker.C, logger)
	return nil
}

// runReaper is the loop of RunReaper with the timer passed in, so tests can
// drive it.
func (q *Queue) runReaper(ctx context.Context, tick <-chan time.Time, logger *slog.Logger) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	for {
		res, err := q.Reap(ctx)
		for _, id := range res.Requeued {
			logger.Info("queue: lease expired, job queued again", "job", id)
		}
		for _, id := range res.Failed {
			logger.Warn("queue: lease expired, job failed after its last attempt", "job", id)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logger.Error("queue: reaper pass failed, will retry", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
	}
}
