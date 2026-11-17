package queue

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// Clipper is what the queue knows about one clipper.
type Clipper struct {
	// Name is the name the clipper sends with its calls.
	Name string
	// LastSeen is the last time it claimed a job or sent a heartbeat.
	LastSeen time.Time
	// JobID is the job it holds now, or 0 when it holds none. A clipper
	// that holds several, which happens when it restarted and claimed again
	// before its old lease was taken back, is shown with the one it last
	// sent a heartbeat for.
	JobID int64
}

// Clippers lists the clippers the queue has seen, most recently seen first,
// for status output; telling online from offline by LastSeen is up to the
// caller. It only reads, on a read connection.
//
// The queue keeps no table of clippers: the list comes from the clipper and
// last heartbeat time each job keeps of its last holder. A clipper whose
// every job was handed to another clipper afterwards drops out of the list,
// and a claim that found no job queued leaves no trace.
func (q *Queue) Clippers(ctx context.Context) ([]Clipper, error) {
	rows, err := q.store.Reader().QueryContext(ctx, `
		SELECT seen.clipper, seen.last_seen,
			(SELECT j.id FROM jobs j
			 WHERE j.status = ? AND j.clipper = seen.clipper
			 ORDER BY j.last_heartbeat_at DESC, j.id DESC LIMIT 1)
		FROM (
			SELECT clipper, MAX(last_heartbeat_at) AS last_seen FROM jobs
			WHERE clipper <> '' AND last_heartbeat_at IS NOT NULL
			GROUP BY clipper
		) AS seen
		ORDER BY seen.last_seen DESC, seen.clipper`,
		store.JobLeased)
	if err != nil {
		return nil, fmt.Errorf("queue: list clippers: %w", err)
	}
	defer rows.Close()
	var clippers []Clipper
	for rows.Next() {
		var (
			c     Clipper
			seen  string
			jobID sql.NullInt64
		)
		if err := rows.Scan(&c.Name, &seen, &jobID); err != nil {
			return nil, fmt.Errorf("queue: list clippers: %w", err)
		}
		if c.LastSeen, err = store.ParseTime(seen); err != nil {
			return nil, fmt.Errorf("queue: list clippers: %w", err)
		}
		c.JobID = jobID.Int64
		clippers = append(clippers, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list clippers: %w", err)
	}
	return clippers, nil
}
