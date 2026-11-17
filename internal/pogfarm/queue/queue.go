package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// Defaults of the Options.
const (
	// DefaultLeaseDuration is how long a claimed job stays with its clipper
	// without a heartbeat.
	DefaultLeaseDuration = 2 * time.Minute
	// DefaultMaxAttempts is how many times a job is handed out before it is
	// given up.
	DefaultMaxAttempts = 3
)

// MinLeaseDuration is the shortest lease New accepts. Below it the heartbeat
// interval, which the contract carries in whole seconds, could not stay well
// under the lease.
const MinLeaseDuration = 4 * time.Second

// heartbeatsPerLease is how many heartbeats fit into one lease when the
// interval is derived from the lease duration: several may be lost before
// the job is.
const heartbeatsPerLease = 4

var (
	// ErrNoJob is returned by Claim when no job is queued.
	ErrNoJob = errors.New("queue: no job queued")
	// ErrUnknownJob means no job has the given id.
	ErrUnknownJob = errors.New("queue: unknown job")
	// ErrLeaseLost means the caller does not hold the job: it is queued
	// again, held by another clipper, or failed.
	ErrLeaseLost = errors.New("queue: lease lost")
	// ErrAlreadyCompleted means the job already has a result.
	ErrAlreadyCompleted = errors.New("queue: job already completed")
	// ErrNoClipper means the caller gave an empty clipper name.
	ErrNoClipper = errors.New("queue: clipper name is empty")
)

// Options configure a Queue. The zero value of every field selects its
// default.
type Options struct {
	// LeaseDuration is how long a claimed job stays with its clipper unless
	// a heartbeat extends the lease. Default DefaultLeaseDuration; at least
	// MinLeaseDuration.
	LeaseDuration time.Duration
	// HeartbeatInterval is how often a clipper is told to send heartbeats.
	// It is a whole number of seconds, at least one, and at most half of
	// LeaseDuration. Default: a quarter of LeaseDuration, cut to the second.
	HeartbeatInterval time.Duration
	// MaxAttempts is how many times a job is handed out before a failure
	// marks it failed instead of queueing it again. Default
	// DefaultMaxAttempts.
	MaxAttempts int
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

// Queue hands out the jobs of one store. It is safe for concurrent use.
type Queue struct {
	store             *store.Store
	now               func() time.Time
	leaseDuration     time.Duration
	heartbeatInterval time.Duration
	maxAttempts       int
}

// New returns the queue on top of st.
func New(st *store.Store, opts Options) (*Queue, error) {
	if st == nil {
		return nil, errors.New("queue: store is nil")
	}
	q := &Queue{
		store:             st,
		now:               opts.Now,
		leaseDuration:     opts.LeaseDuration,
		heartbeatInterval: opts.HeartbeatInterval,
		maxAttempts:       opts.MaxAttempts,
	}
	if q.now == nil {
		q.now = time.Now
	}
	if q.leaseDuration == 0 {
		q.leaseDuration = DefaultLeaseDuration
	}
	if q.leaseDuration < MinLeaseDuration {
		return nil, fmt.Errorf("queue: lease duration %s is below the minimum of %s", q.leaseDuration, MinLeaseDuration)
	}
	if q.heartbeatInterval == 0 {
		q.heartbeatInterval = (q.leaseDuration / heartbeatsPerLease).Truncate(time.Second)
	}
	if q.heartbeatInterval < time.Second || q.heartbeatInterval%time.Second != 0 ||
		q.heartbeatInterval > q.leaseDuration/2 {
		return nil, fmt.Errorf(
			"queue: heartbeat interval %s must be whole seconds, at least 1s and at most half the lease duration %s",
			q.heartbeatInterval, q.leaseDuration)
	}
	if q.maxAttempts == 0 {
		q.maxAttempts = DefaultMaxAttempts
	}
	if q.maxAttempts < 0 {
		return nil, fmt.Errorf("queue: max attempts %d is negative", q.maxAttempts)
	}
	return q, nil
}

// LeaseDuration returns how long a lease lasts without a heartbeat.
func (q *Queue) LeaseDuration() time.Duration { return q.leaseDuration }

// HeartbeatInterval returns how often clippers are told to send heartbeats.
func (q *Queue) HeartbeatInterval() time.Duration { return q.heartbeatInterval }

// MaxAttempts returns how many times a job is handed out before it is given
// up.
func (q *Queue) MaxAttempts() int { return q.maxAttempts }

// JobID returns a job's id as the contract carries it.
func JobID(id int64) string { return strconv.FormatInt(id, 10) }

// ParseJobID reads a job id as the contract carries it. A string that cannot
// be the id of any job is ErrUnknownJob.
func ParseJobID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	// JobID never writes a sign or leading zeros, so those are not ids.
	if err != nil || id <= 0 || JobID(id) != s {
		return 0, fmt.Errorf("%w: %q", ErrUnknownJob, s)
	}
	return id, nil
}

// stamp returns the queue's current time as it reads back from the database.
func (q *Queue) stamp() time.Time {
	return q.now().UTC().Truncate(time.Millisecond)
}

// getJob loads a job inside a transaction. A missing job is ErrUnknownJob.
func getJob(ctx context.Context, tx *sql.Tx, id int64) (store.Job, error) {
	job, err := store.GetJob(ctx, tx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Job{}, fmt.Errorf("%w: %d", ErrUnknownJob, id)
	}
	if err != nil {
		return store.Job{}, fmt.Errorf("queue: %w", err)
	}
	return job, nil
}
