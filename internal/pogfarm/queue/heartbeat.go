package queue

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// Heartbeat extends the lease of the job clipper holds: the lease now runs
// for the queue's lease duration from now, and now is recorded as the job's
// last heartbeat. It returns the new lease expiry, as stored.
//
// Who holds the job is decided as for Fail. A lease that has run out but has
// not been taken back by the reaper yet still counts, and the heartbeat
// renews it. Nothing changes when there is no such job (ErrUnknownJob), when
// the job already has a result (ErrAlreadyCompleted) or when the caller does
// not hold it (ErrLeaseLost): the job is queued again, held by another
// clipper, or failed.
func (q *Queue) Heartbeat(ctx context.Context, jobID int64, clipper string) (time.Time, error) {
	if clipper == "" {
		return time.Time{}, ErrNoClipper
	}
	var expires time.Time
	err := q.store.WriteTx(ctx, func(tx *sql.Tx) error {
		job, err := getJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if err := checkHolder(job, clipper); err != nil {
			return err
		}
		now := q.stamp()
		expires = now.Add(q.leaseDuration).Truncate(time.Millisecond)
		res, err := tx.ExecContext(ctx, `
			UPDATE jobs SET lease_expires_at = ?, last_heartbeat_at = ?, updated_at = ?
			WHERE id = ? AND status = ? AND clipper = ?`,
			store.FormatTime(expires), store.FormatTime(now), store.FormatTime(now),
			job.ID, store.JobLeased, clipper)
		if err != nil {
			return fmt.Errorf("queue: heartbeat job %d: %w", job.ID, err)
		}
		// The row was read in this transaction, so this cannot happen; the
		// check keeps the statement honest if that ever changes.
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("queue: heartbeat job %d: %w", job.ID, err)
		} else if n == 0 {
			return fmt.Errorf("%w: job %d", ErrLeaseLost, job.ID)
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return expires, nil
}
