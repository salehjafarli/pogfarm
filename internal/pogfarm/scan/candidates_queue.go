package scan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// QueueResult counts what QueueCandidates created.
type QueueResult struct {
	// Candidates is the number of candidate rows created.
	Candidates int
	// Jobs is the number of render jobs created.
	Jobs int
}

// QueueCandidates stores the candidates of a VOD, as BuildCandidates
// returns them, and enqueues one render job for each candidate with Keep
// set. Candidates without Keep are stored as store.CandidateSkipped and get
// no job. Every row carries the VOD's platform.
//
// Everything happens in one transaction of st, and q must be a queue on st.
// It is safe to run again for the same VOD: a window that is already stored
// is not stored again (the candidates table is unique on VOD and window),
// and a kept candidate that already has a job does not get another. Running
// again with the same candidates therefore creates nothing.
func QueueCandidates(ctx context.Context, st *store.Store, q *queue.Queue, vod store.VOD, cands []Candidate) (QueueResult, error) {
	if vod.ID == 0 || vod.Platform == "" {
		return QueueResult{}, errors.New("scan: queue candidates: the VOD has no id or no platform")
	}
	var res QueueResult
	err := st.WriteTx(ctx, func(tx *sql.Tx) error {
		res = QueueResult{}
		for _, c := range cands {
			row := store.Candidate{
				Platform:     vod.Platform,
				VODID:        vod.ID,
				StartSeconds: c.Start.Seconds(),
				EndSeconds:   c.End.Seconds(),
				Score:        c.Score,
				Signal:       c.Signal,
				Status:       store.CandidateNew,
			}
			if !c.Keep {
				row.Status = store.CandidateSkipped
			}
			stored, err := store.InsertCandidate(ctx, tx, row)
			switch {
			case err == nil:
				res.Candidates++
			case errors.Is(err, store.ErrDuplicate):
				if !c.Keep {
					continue
				}
				// Stored by an earlier run; make sure it has its job.
				if stored.ID, err = candStoredID(ctx, tx, row); err != nil {
					return err
				}
			default:
				return fmt.Errorf("scan: queue candidates of vod %d: %w", vod.ID, err)
			}
			if !c.Keep {
				continue
			}
			_, created, err := q.EnqueueTx(ctx, tx, stored.ID)
			if err != nil {
				return fmt.Errorf("scan: queue candidates of vod %d: %w", vod.ID, err)
			}
			if created {
				res.Jobs++
			}
		}
		return nil
	})
	if err != nil {
		return QueueResult{}, err
	}
	return res, nil
}

// candStoredID finds the id of the stored candidate with the VOD and window
// of c.
func candStoredID(ctx context.Context, tx *sql.Tx, c store.Candidate) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM candidates WHERE vod_id = ? AND start_seconds = ? AND end_seconds = ?`,
		c.VODID, c.StartSeconds, c.EndSeconds).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("scan: queue candidates of vod %d: find stored window [%v, %v): %w",
			c.VODID, c.StartSeconds, c.EndSeconds, err)
	}
	return id, nil
}
