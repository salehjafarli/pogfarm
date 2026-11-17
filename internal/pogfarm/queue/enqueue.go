package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// Enqueue creates the queued render job of a candidate and marks the
// candidate queued. A candidate has at most one job: when it already has
// one, that job is returned unchanged and created is false, so enqueueing
// twice is harmless. A candidate that does not exist is store.ErrNotFound.
func (q *Queue) Enqueue(ctx context.Context, candidateID int64) (job store.Job, created bool, err error) {
	err = q.store.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		job, created, err = q.EnqueueTx(ctx, tx, candidateID)
		return err
	})
	if err != nil {
		return store.Job{}, false, err
	}
	return job, created, nil
}

// EnqueueTx is Enqueue inside a transaction the caller already holds (from
// Store.WriteTx), for callers that store candidates and queue their jobs
// together.
func (q *Queue) EnqueueTx(ctx context.Context, tx *sql.Tx, candidateID int64) (job store.Job, created bool, err error) {
	cand, err := store.GetCandidate(ctx, tx, candidateID)
	if err != nil {
		return store.Job{}, false, fmt.Errorf("queue: enqueue candidate %d: %w", candidateID, err)
	}
	job, err = store.GetJobByCandidate(ctx, tx, candidateID)
	if err == nil {
		return job, false, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Job{}, false, fmt.Errorf("queue: enqueue candidate %d: %w", candidateID, err)
	}

	now := q.stamp()
	job, err = store.InsertJob(ctx, tx, store.Job{
		Platform:    cand.Platform,
		CandidateID: cand.ID,
		Status:      store.JobQueued,
		CreatedAt:   now,
	})
	if err != nil {
		return store.Job{}, false, fmt.Errorf("queue: enqueue candidate %d: %w", candidateID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE candidates SET status = ?, updated_at = ? WHERE id = ?`,
		store.CandidateQueued, store.FormatTime(now), cand.ID); err != nil {
		return store.Job{}, false, fmt.Errorf("queue: enqueue candidate %d: mark queued: %w", candidateID, err)
	}
	return job, true, nil
}
