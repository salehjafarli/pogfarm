package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/salehjafarli/pogfarm/internal/api"
)

// ErrNotFound is returned (wrapped) by the Get functions when no row matches
// and by the Update functions when the row to update does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrDuplicate is returned (wrapped) by the Insert and Update functions when
// the row would break a UNIQUE constraint: the same VOD recorded twice, a
// second clip for a job, a second upload of a clip to the same target, and so
// on. Nothing is written.
var ErrDuplicate = errors.New("store: duplicate")

// TimeLayout is the format of every time stored in the database: UTC with
// millisecond precision and a fixed width, so that comparing two stored
// times as strings in SQL compares them as times.
const TimeLayout = "2006-01-02T15:04:05.000Z"

// FormatTime returns t as it is stored in the database. Use it for every
// time passed to a hand-written query.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeLayout)
}

// ParseTime reads a time stored in the database.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(TimeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: stored time %q: %w", s, err)
	}
	return t.UTC(), nil
}

// Querier is what the query functions run on: Store.Reader, Store.Writer or
// the transaction of Store.WriteTx. Functions that change data need the
// writer or a transaction.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ScanStatus is the scan state of a VOD (vods.scan_status).
type ScanStatus string

const (
	// ScanNew: recorded, waiting for the scanner.
	ScanNew ScanStatus = "new"
	// ScanScanning: the scanner is working on it.
	ScanScanning ScanStatus = "scanning"
	// ScanScanned: candidates have been built.
	ScanScanned ScanStatus = "scanned"
	// ScanFailed: the last scan failed; see VOD.ScanError.
	ScanFailed ScanStatus = "failed"
)

// Signal says what made a moment a candidate (candidates.signal).
type Signal string

const (
	SignalChatSpike  Signal = "chat_spike"
	SignalViewerClip Signal = "viewer_clip"
	// SignalBoth: a chat spike and a viewer clip confirm the same moment.
	SignalBoth Signal = "both"
)

// CandidateStatus is the state of a candidate (candidates.status).
type CandidateStatus string

const (
	// CandidateNew: stored, nothing decided yet.
	CandidateNew CandidateStatus = "new"
	// CandidateQueued: a render job exists for it.
	CandidateQueued CandidateStatus = "queued"
	// CandidateSkipped: ranked below the per-VOD limit; never rendered.
	CandidateSkipped CandidateStatus = "skipped"
)

// JobStatus is the state of a render job (jobs.status).
type JobStatus string

const (
	JobQueued JobStatus = "queued"
	// JobLeased: a clipper holds the job until Job.LeaseExpiresAt.
	JobLeased JobStatus = "leased"
	JobDone   JobStatus = "done"
	JobFailed JobStatus = "failed"
)

// ReviewStatus is the owner's decision on a clip (clips.review_status).
type ReviewStatus string

const (
	ReviewPending  ReviewStatus = "pending"
	ReviewApproved ReviewStatus = "approved"
	ReviewRejected ReviewStatus = "rejected"
)

// UploadStatus is the state of one post of a clip (uploads.status).
type UploadStatus string

const (
	UploadPending UploadStatus = "pending"
	// UploadUploading: the target is being called. A row left here by a
	// crash may or may not have been posted.
	UploadUploading UploadStatus = "uploading"
	UploadDone      UploadStatus = "done"
	UploadFailed    UploadStatus = "failed"
)

// The row types below mirror the tables one to one. In all of them:
//
//   - ID, CreatedAt and UpdatedAt are set by the Insert function. A non-zero
//     CreatedAt passed to Insert is kept; UpdatedAt is set by every Update.
//   - Times come back in UTC, cut to the millisecond. An optional time is the
//     zero time when not set.
//   - A status left empty on Insert takes the table's starting status.

// Streamer is a row of streamers: an account on the allowlist.
type Streamer struct {
	ID       int64
	Platform api.Platform
	// PlatformUserID is the platform's permanent id of the account. With
	// Platform it identifies the streamer.
	PlatformUserID string
	Login          string
	DisplayName    string
	Enabled        bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// VOD is a row of vods: one recorded stream that has ended.
type VOD struct {
	ID       int64
	Platform api.Platform
	// PlatformVODID is the platform's id of the VOD. With Platform it
	// identifies the VOD.
	PlatformVODID   string
	StreamerID      int64
	Title           string
	DurationSeconds float64
	StartedAt       time.Time
	EndedAt         time.Time
	ScanStatus      ScanStatus
	// ScanAttempts counts the scans started so far.
	ScanAttempts int
	// ScanError is why the last scan failed.
	ScanError string
	// NextScanAt is the earliest time of the next scan attempt.
	NextScanAt time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Candidate is a row of candidates: a moment of a VOD worth rendering.
type Candidate struct {
	ID       int64
	Platform api.Platform
	VODID    int64
	// StartSeconds and EndSeconds are offsets from the start of the VOD.
	StartSeconds float64
	EndSeconds   float64
	Score        float64
	Signal       Signal
	Status       CandidateStatus
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Job is a row of jobs: the render job of one candidate.
type Job struct {
	ID          int64
	Platform    api.Platform
	CandidateID int64
	Status      JobStatus
	// Attempts counts how many times the job has been handed out.
	Attempts int
	// Clipper is the clipper that holds the job, or held it last.
	Clipper string
	// LeaseExpiresAt is set exactly while Status is JobLeased.
	LeaseExpiresAt time.Time
	// LastHeartbeatAt is when the holder last claimed the job or sent a
	// heartbeat for it.
	LastHeartbeatAt time.Time
	LastError       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Clip is a row of clips: the accepted result of a job.
type Clip struct {
	ID       int64
	Platform api.Platform
	JobID    int64
	// FilePath is relative to the data directory.
	FilePath string
	// SHA256 is the lowercase hex SHA-256 of the file.
	SHA256          string
	SizeBytes       int64
	DurationSeconds float64
	Width           int
	Height          int
	// TranscriptJSON is the contract's Transcript document.
	TranscriptJSON string
	// Clipper is the clipper whose result was accepted.
	Clipper      string
	ReviewStatus ReviewStatus
	// ReviewedAt is set exactly when ReviewStatus is not ReviewPending.
	ReviewedAt time.Time
	// TelegramChatID and TelegramMessageID locate the review message. Both
	// are 0 until the clip has been sent.
	TelegramChatID    int64
	TelegramMessageID int64
	// FileRemovedAt is when cleanup deleted the file.
	FileRemovedAt time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Upload is a row of uploads: one post of a clip to one target.
type Upload struct {
	ID     int64
	ClipID int64
	// TargetPlatform names where the clip is posted, not where the VOD
	// came from.
	TargetPlatform string
	Status         UploadStatus
	ExternalPostID string
	Attempts       int
	LastError      string
	// NextAttemptAt is the earliest time of the next attempt.
	NextAttemptAt time.Time
	// PostedAt is set exactly when Status is UploadDone.
	PostedAt  time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// stored returns t as it reads back from the database.
func stored(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC().Truncate(time.Millisecond)
}

// stamps returns the created and updated times of a row being inserted.
func stamps(created time.Time) (time.Time, time.Time) {
	if created.IsZero() {
		created = time.Now()
	}
	created = stored(created)
	return created, created
}

// timeArg is a required time as a query argument.
func timeArg(t time.Time) string { return FormatTime(t) }

// optTimeArg is an optional time as a query argument: NULL when zero.
func optTimeArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return FormatTime(t)
}

// optIDArg is an optional id as a query argument: NULL when zero.
func optIDArg(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// timeCol scans a required time column.
type timeCol struct{ t *time.Time }

func (c timeCol) Scan(src any) error {
	s, ok := src.(string)
	if !ok {
		return fmt.Errorf("store: time column holds %T, want text", src)
	}
	t, err := ParseTime(s)
	if err != nil {
		return err
	}
	*c.t = t
	return nil
}

// optTimeCol scans an optional time column: NULL becomes the zero time.
type optTimeCol struct{ t *time.Time }

func (c optTimeCol) Scan(src any) error {
	if src == nil {
		*c.t = time.Time{}
		return nil
	}
	return timeCol(c).Scan(src)
}

// optIDCol scans an optional id column: NULL becomes 0.
type optIDCol struct{ id *int64 }

func (c optIDCol) Scan(src any) error {
	var n sql.NullInt64
	if err := n.Scan(src); err != nil {
		return err
	}
	*c.id = n.Int64
	return nil
}

// wrap turns a database error into one that names the operation and, where
// it applies, wraps ErrNotFound or ErrDuplicate.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: %s: %w", op, ErrNotFound)
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("store: %s: %w: %w", op, ErrDuplicate, err)
		}
	}
	return fmt.Errorf("store: %s: %w", op, err)
}

// insert runs an INSERT and returns the new row's id.
func insert(ctx context.Context, q Querier, op, query string, args ...any) (int64, error) {
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, wrap(op, err)
	}
	id, err := res.LastInsertId()
	return id, wrap(op, err)
}

// update runs an UPDATE of one row and reports ErrNotFound when it matched
// nothing.
func update(ctx context.Context, q Querier, op, query string, args ...any) error {
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return wrap(op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrap(op, err)
	}
	if n == 0 {
		return fmt.Errorf("store: %s: %w", op, ErrNotFound)
	}
	return nil
}
