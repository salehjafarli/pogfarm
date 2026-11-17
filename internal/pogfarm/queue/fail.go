package queue

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// Fail records that clipper could not finish the job it holds. The job is
// queued again when retryable is true and it has been handed out fewer than
// MaxAttempts times; otherwise it is marked failed. Either way reason is
// kept as the job's last error. The returned row is the job after the
// change.
//
// The report is ignored, and nothing changes, when there is no such job
// (ErrUnknownJob), when the job already has a result (ErrAlreadyCompleted)
// or when the caller does not hold it (ErrLeaseLost).
func (q *Queue) Fail(ctx context.Context, jobID int64, clipper, reason string, retryable bool) (store.Job, error) {
	if clipper == "" {
		return store.Job{}, ErrNoClipper
	}
	var job store.Job
	err := q.store.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		if job, err = getJob(ctx, tx, jobID); err != nil {
			return err
		}
		if err := checkHolder(job, clipper); err != nil {
			return err
		}
		job, err = q.release(ctx, tx, job, reason, retryable)
		return err
	})
	if err != nil {
		return store.Job{}, err
	}
	return job, nil
}

// checkHolder reports whether clipper holds the lease of job. A lease that
// has run out but has not been taken back yet still counts: until then
// nobody else was given the job.
func checkHolder(job store.Job, clipper string) error {
	switch {
	case job.Status == store.JobDone:
		return fmt.Errorf("%w: job %d", ErrAlreadyCompleted, job.ID)
	case job.Status != store.JobLeased:
		return fmt.Errorf("%w: job %d is %s", ErrLeaseLost, job.ID, job.Status)
	case job.Clipper != clipper:
		return fmt.Errorf("%w: job %d is held by another clipper", ErrLeaseLost, job.ID)
	}
	return nil
}

// release ends the lease of a leased job and returns the row as stored: the
// job goes back to queued when retry is true and attempts remain, and to
// failed otherwise. It is the one place that applies the attempt limit. The
// clipper and its last heartbeat time stay on the row, as who held it last.
func (q *Queue) release(ctx context.Context, tx *sql.Tx, job store.Job, reason string, retry bool) (store.Job, error) {
	status := store.JobFailed
	if retry && job.Attempts < q.maxAttempts {
		status = store.JobQueued
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE jobs SET status = ?, lease_expires_at = NULL, last_error = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		status, reason, store.FormatTime(q.stamp()), job.ID, store.JobLeased)
	if err != nil {
		return store.Job{}, fmt.Errorf("queue: release job %d: %w", job.ID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return store.Job{}, fmt.Errorf("queue: release job %d: %w", job.ID, err)
	} else if n == 0 {
		return store.Job{}, fmt.Errorf("%w: job %d is not leased", ErrLeaseLost, job.ID)
	}
	return getJob(ctx, tx, job.ID)
}
