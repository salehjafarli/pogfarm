// Package status reads the state of the farm for the owner's status
// commands: how many jobs, VODs, clips and uploads are in each state, the
// clips awaiting review, and the clippers seen recently. It only reads, on
// the store's read connections, and never changes anything.
package status

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// offlineLeases is how many lease durations a clipper may stay silent
// before it is shown as offline, when Options.OfflineAfter is not set. A
// clipper that holds a job sends a heartbeat every quarter lease, so two
// leases is eight missed heartbeats.
const offlineLeases = 2

// Options configure a Reader. The zero value of every field selects its
// default.
type Options struct {
	// OfflineAfter is how long a clipper may go without a claim or a
	// heartbeat before it is shown as offline. Default: twice the queue's
	// lease duration.
	OfflineAfter time.Duration
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

// Reader answers status questions about one store.
type Reader struct {
	store        *store.Store
	queue        *queue.Queue
	offlineAfter time.Duration
	now          func() time.Time
}

// New returns a reader of st, with clipper liveness from q.
func New(st *store.Store, q *queue.Queue, opts Options) (*Reader, error) {
	if st == nil || q == nil {
		return nil, errors.New("status: store and queue are required")
	}
	if opts.OfflineAfter < 0 {
		return nil, fmt.Errorf("status: offline threshold %s is negative", opts.OfflineAfter)
	}
	r := &Reader{store: st, queue: q, offlineAfter: opts.OfflineAfter, now: opts.Now}
	if r.offlineAfter == 0 {
		r.offlineAfter = offlineLeases * q.LeaseDuration()
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r, nil
}

// OfflineAfter returns how long a clipper may stay silent before it is shown
// as offline.
func (r *Reader) OfflineAfter() time.Duration { return r.offlineAfter }

// Now returns the reader's current time.
func (r *Reader) Now() time.Time { return r.now() }

// Jobs counts render jobs by state.
type Jobs struct {
	Queued int
	Leased int
	// DoneToday are the jobs completed since midnight UTC.
	DoneToday int
	Failed    int
}

// VODs counts VODs by scan state. VODs that were scanned are not counted.
type VODs struct {
	// New are found and waiting for their first scan.
	New      int
	Scanning int
	// Retrying failed a scan and will be scanned again.
	Retrying int
	// Failed failed for good: they ran out of attempts.
	Failed int
}

// Clips counts clips awaiting review.
type Clips struct {
	// AwaitingReview are pending review, sent or not.
	AwaitingReview int
	// Unsent are those of them with no review message yet.
	Unsent int
}

// Uploads counts uploads that are not done.
type Uploads struct {
	Pending   int
	Uploading int
	Failed    int
}

// Clipper is one clipper the queue has seen.
type Clipper struct {
	Name     string
	LastSeen time.Time
	// JobID is the job it holds now, or 0.
	JobID int64
	// Online is false when it was last seen longer ago than the offline
	// threshold.
	Online bool
}

// Snapshot is the state of the farm at one moment.
type Snapshot struct {
	// At is when it was taken.
	At      time.Time
	Jobs    Jobs
	VODs    VODs
	Clips   Clips
	Uploads Uploads
	// Clippers are most recently seen first. The queue keeps no table of
	// clippers: one whose every job went to another clipper since is not
	// listed, and a claim that found no job leaves no trace, so an idle
	// clipper that polls an empty queue shows as offline.
	Clippers []Clipper
	// OfflineAfter is the threshold the clippers were judged by.
	OfflineAfter time.Duration
}

// StartOfDay returns midnight UTC of the day t falls on: where "done today"
// starts.
func StartOfDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Snapshot reads the counts and the clippers.
func (r *Reader) Snapshot(ctx context.Context) (Snapshot, error) {
	now := r.now()
	s := Snapshot{At: now, OfflineAfter: r.offlineAfter}
	db := r.store.Reader()
	// Every count is one query that the status indexes serve; they are not
	// one transaction, so counts taken while work moves on may be a moment
	// apart, which status output can live with.
	err := db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM jobs WHERE status = ?),
			(SELECT COUNT(*) FROM jobs WHERE status = ?),
			(SELECT COUNT(*) FROM jobs WHERE status = ? AND updated_at >= ? AND updated_at < ?),
			(SELECT COUNT(*) FROM jobs WHERE status = ?),
			(SELECT COUNT(*) FROM vods WHERE scan_status = ?),
			(SELECT COUNT(*) FROM vods WHERE scan_status = ?),
			(SELECT COUNT(*) FROM vods WHERE scan_status = ? AND next_scan_at IS NOT NULL),
			(SELECT COUNT(*) FROM vods WHERE scan_status = ? AND next_scan_at IS NULL),
			(SELECT COUNT(*) FROM clips WHERE review_status = ?),
			(SELECT COUNT(*) FROM clips WHERE review_status = ? AND telegram_message_id IS NULL),
			(SELECT COUNT(*) FROM uploads WHERE status = ?),
			(SELECT COUNT(*) FROM uploads WHERE status = ?),
			(SELECT COUNT(*) FROM uploads WHERE status = ?)`,
		store.JobQueued, store.JobLeased,
		store.JobDone, store.FormatTime(StartOfDay(now)), store.FormatTime(StartOfDay(now).AddDate(0, 0, 1)),
		store.JobFailed,
		store.ScanNew, store.ScanScanning, store.ScanFailed, store.ScanFailed,
		store.ReviewPending, store.ReviewPending,
		store.UploadPending, store.UploadUploading, store.UploadFailed,
	).Scan(
		&s.Jobs.Queued, &s.Jobs.Leased, &s.Jobs.DoneToday, &s.Jobs.Failed,
		&s.VODs.New, &s.VODs.Scanning, &s.VODs.Retrying, &s.VODs.Failed,
		&s.Clips.AwaitingReview, &s.Clips.Unsent,
		&s.Uploads.Pending, &s.Uploads.Uploading, &s.Uploads.Failed,
	)
	if err != nil {
		return s, fmt.Errorf("status: count: %w", err)
	}

	clippers, err := r.queue.Clippers(ctx)
	if err != nil {
		return s, fmt.Errorf("status: %w", err)
	}
	for _, c := range clippers {
		s.Clippers = append(s.Clippers, Clipper{
			Name:     c.Name,
			LastSeen: c.LastSeen,
			JobID:    c.JobID,
			Online:   now.Sub(c.LastSeen) <= r.offlineAfter,
		})
	}
	return s, nil
}

// PendingClip is a clip awaiting review.
type PendingClip struct {
	ID int64
	// Streamer is the display name, or the login when that is empty.
	Streamer  string
	VODTitle  string
	CreatedAt time.Time
	// Sent is true when the clip has a review message.
	Sent bool
}

// Pending returns up to limit clips awaiting review, oldest first, and how
// many there are in all.
func (r *Reader) Pending(ctx context.Context, limit int) (clips []PendingClip, total int, err error) {
	if limit < 0 {
		limit = 0
	}
	db := r.store.Reader()
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM clips WHERE review_status = ?`,
		store.ReviewPending).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("status: count pending clips: %w", err)
	}
	if total == 0 || limit == 0 {
		return nil, total, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT c.id, s.display_name, s.login, v.title, c.created_at, c.telegram_message_id
		FROM clips c
		JOIN jobs j ON j.id = c.job_id
		JOIN candidates cand ON cand.id = j.candidate_id
		JOIN vods v ON v.id = cand.vod_id
		JOIN streamers s ON s.id = v.streamer_id
		WHERE c.review_status = ?
		ORDER BY c.created_at, c.id
		LIMIT ?`, store.ReviewPending, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("status: list pending clips: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			c              PendingClip
			display, login string
			created        string
			messageID      sql.NullInt64
		)
		if err := rows.Scan(&c.ID, &display, &login, &c.VODTitle, &created, &messageID); err != nil {
			return nil, 0, fmt.Errorf("status: list pending clips: %w", err)
		}
		if c.CreatedAt, err = store.ParseTime(created); err != nil {
			return nil, 0, fmt.Errorf("status: list pending clips: %w", err)
		}
		c.Streamer = display
		if c.Streamer == "" {
			c.Streamer = login
		}
		c.Sent = messageID.Valid
		clips = append(clips, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("status: list pending clips: %w", err)
	}
	return clips, total, nil
}
