package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// Claim hands the oldest queued job to clipper: the job becomes leased to
// it, its attempt count goes up by one and its lease runs for the queue's
// lease duration from now. The result is the contract's job payload. When
// nothing is queued Claim returns ErrNoJob.
//
// Picking the job and marking it leased are one statement in one write
// transaction, so concurrent callers always receive different jobs.
func (q *Queue) Claim(ctx context.Context, clipper string) (api.Job, error) {
	if clipper == "" {
		return api.Job{}, ErrNoClipper
	}
	var job api.Job
	err := q.store.WriteTx(ctx, func(tx *sql.Tx) error {
		now := q.stamp()
		expires := now.Add(q.leaseDuration)

		var (
			id, candidateID int64
			attempts        int
		)
		// jobs_status_created serves the subquery: oldest first, and the id
		// decides between jobs created in the same millisecond.
		err := tx.QueryRowContext(ctx, `
			UPDATE jobs
			SET status = ?, attempts = attempts + 1, clipper = ?, lease_expires_at = ?,
				last_heartbeat_at = ?, updated_at = ?
			WHERE id = (SELECT id FROM jobs WHERE status = ? ORDER BY created_at, id LIMIT 1)
			RETURNING id, candidate_id, attempts`,
			store.JobLeased, clipper, store.FormatTime(expires), store.FormatTime(now), store.FormatTime(now),
			store.JobQueued,
		).Scan(&id, &candidateID, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoJob
		}
		if err != nil {
			return fmt.Errorf("queue: claim: %w", err)
		}

		cand, err := store.GetCandidate(ctx, tx, candidateID)
		if err != nil {
			return fmt.Errorf("queue: claim job %d: %w", id, err)
		}
		vod, err := store.GetVOD(ctx, tx, cand.VODID)
		if err != nil {
			return fmt.Errorf("queue: claim job %d: %w", id, err)
		}
		streamer, err := store.GetStreamer(ctx, tx, vod.StreamerID)
		if err != nil {
			return fmt.Errorf("queue: claim job %d: %w", id, err)
		}
		job = api.Job{
			ID:           JobID(id),
			Attempt:      attempts,
			Platform:     vod.Platform,
			VODID:        vod.PlatformVODID,
			StartSeconds: cand.StartSeconds,
			EndSeconds:   cand.EndSeconds,
			Streamer: api.Streamer{
				Login:       streamer.Login,
				DisplayName: streamer.DisplayName,
			},
			LeaseExpiresAt:           expires,
			HeartbeatIntervalSeconds: int(q.heartbeatInterval / time.Second),
		}
		return nil
	})
	if err != nil {
		return api.Job{}, err
	}
	return job, nil
}
