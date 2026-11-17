package store

import (
	"context"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
)

// The functions in this file are the plain insert, get and update of each
// table. Each runs one statement on the Querier it is given, so several can
// be combined in one Store.WriteTx. Conditional changes (claiming a job,
// deciding a review) are single SQL statements of their own and live with
// the code that owns them.

// ---- streamers ----

const streamerCols = `id, platform, platform_user_id, login, display_name, enabled, created_at, updated_at`

func getStreamer(ctx context.Context, q Querier, op, where string, args ...any) (Streamer, error) {
	var s Streamer
	err := q.QueryRowContext(ctx, `SELECT `+streamerCols+` FROM streamers WHERE `+where, args...).Scan(
		&s.ID, &s.Platform, &s.PlatformUserID, &s.Login, &s.DisplayName, &s.Enabled,
		timeCol{&s.CreatedAt}, timeCol{&s.UpdatedAt})
	return s, wrap(op, err)
}

// InsertStreamer adds a streamer and returns the stored row. A streamer with
// the same platform and platform user id is ErrDuplicate.
func InsertStreamer(ctx context.Context, q Querier, s Streamer) (Streamer, error) {
	s.CreatedAt, s.UpdatedAt = stamps(s.CreatedAt)
	id, err := insert(ctx, q, "insert streamer", `
		INSERT INTO streamers (platform, platform_user_id, login, display_name, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.Platform, s.PlatformUserID, s.Login, s.DisplayName, s.Enabled,
		timeArg(s.CreatedAt), timeArg(s.UpdatedAt))
	s.ID = id
	return s, err
}

// GetStreamer returns the streamer with the given row id.
func GetStreamer(ctx context.Context, q Querier, id int64) (Streamer, error) {
	return getStreamer(ctx, q, "get streamer", `id = ?`, id)
}

// GetStreamerByPlatformID returns the streamer with the given platform user
// id.
func GetStreamerByPlatformID(ctx context.Context, q Querier, platform api.Platform, platformUserID string) (Streamer, error) {
	return getStreamer(ctx, q, "get streamer", `platform = ? AND platform_user_id = ?`, platform, platformUserID)
}

// UpdateStreamer stores the login, display name and enabled flag of s. The
// platform and platform user id of a streamer never change.
func UpdateStreamer(ctx context.Context, q Querier, s Streamer) error {
	return update(ctx, q, "update streamer", `
		UPDATE streamers SET login = ?, display_name = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		s.Login, s.DisplayName, s.Enabled, timeArg(time.Now()), s.ID)
}

// ---- vods ----

const vodCols = `id, platform, platform_vod_id, streamer_id, title, duration_seconds, started_at, ended_at,
	scan_status, scan_attempts, scan_error, next_scan_at, created_at, updated_at`

func getVOD(ctx context.Context, q Querier, op, where string, args ...any) (VOD, error) {
	var v VOD
	err := q.QueryRowContext(ctx, `SELECT `+vodCols+` FROM vods WHERE `+where, args...).Scan(
		&v.ID, &v.Platform, &v.PlatformVODID, &v.StreamerID, &v.Title, &v.DurationSeconds,
		timeCol{&v.StartedAt}, timeCol{&v.EndedAt},
		&v.ScanStatus, &v.ScanAttempts, &v.ScanError, optTimeCol{&v.NextScanAt},
		timeCol{&v.CreatedAt}, timeCol{&v.UpdatedAt})
	return v, wrap(op, err)
}

// InsertVOD records a VOD and returns the stored row. An empty ScanStatus
// becomes ScanNew. A VOD with the same platform and platform VOD id is
// ErrDuplicate and leaves the existing row untouched. The platform must be
// the streamer's.
func InsertVOD(ctx context.Context, q Querier, v VOD) (VOD, error) {
	if v.ScanStatus == "" {
		v.ScanStatus = ScanNew
	}
	v.StartedAt, v.EndedAt, v.NextScanAt = stored(v.StartedAt), stored(v.EndedAt), stored(v.NextScanAt)
	v.CreatedAt, v.UpdatedAt = stamps(v.CreatedAt)
	id, err := insert(ctx, q, "insert vod", `
		INSERT INTO vods (platform, platform_vod_id, streamer_id, title, duration_seconds, started_at, ended_at,
			scan_status, scan_attempts, scan_error, next_scan_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.Platform, v.PlatformVODID, v.StreamerID, v.Title, v.DurationSeconds,
		timeArg(v.StartedAt), timeArg(v.EndedAt),
		v.ScanStatus, v.ScanAttempts, v.ScanError, optTimeArg(v.NextScanAt),
		timeArg(v.CreatedAt), timeArg(v.UpdatedAt))
	v.ID = id
	return v, err
}

// GetVOD returns the VOD with the given row id.
func GetVOD(ctx context.Context, q Querier, id int64) (VOD, error) {
	return getVOD(ctx, q, "get vod", `id = ?`, id)
}

// GetVODByPlatformID returns the VOD with the given platform VOD id.
func GetVODByPlatformID(ctx context.Context, q Querier, platform api.Platform, platformVODID string) (VOD, error) {
	return getVOD(ctx, q, "get vod", `platform = ? AND platform_vod_id = ?`, platform, platformVODID)
}

// UpdateVOD stores the title, duration, start and end times and the scan
// fields of v. The identity and the streamer of a VOD never change.
func UpdateVOD(ctx context.Context, q Querier, v VOD) error {
	return update(ctx, q, "update vod", `
		UPDATE vods SET title = ?, duration_seconds = ?, started_at = ?, ended_at = ?,
			scan_status = ?, scan_attempts = ?, scan_error = ?, next_scan_at = ?, updated_at = ?
		WHERE id = ?`,
		v.Title, v.DurationSeconds, timeArg(v.StartedAt), timeArg(v.EndedAt),
		v.ScanStatus, v.ScanAttempts, v.ScanError, optTimeArg(v.NextScanAt), timeArg(time.Now()), v.ID)
}

// ---- candidates ----

const candidateCols = `id, platform, vod_id, start_seconds, end_seconds, score, signal, status, created_at, updated_at`

// InsertCandidate stores a candidate and returns the stored row. An empty
// Status becomes CandidateNew. A candidate with the same VOD and the same
// window is ErrDuplicate. The platform must be the VOD's.
func InsertCandidate(ctx context.Context, q Querier, c Candidate) (Candidate, error) {
	if c.Status == "" {
		c.Status = CandidateNew
	}
	c.CreatedAt, c.UpdatedAt = stamps(c.CreatedAt)
	id, err := insert(ctx, q, "insert candidate", `
		INSERT INTO candidates (platform, vod_id, start_seconds, end_seconds, score, signal, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Platform, c.VODID, c.StartSeconds, c.EndSeconds, c.Score, c.Signal, c.Status,
		timeArg(c.CreatedAt), timeArg(c.UpdatedAt))
	c.ID = id
	return c, err
}

// GetCandidate returns the candidate with the given row id.
func GetCandidate(ctx context.Context, q Querier, id int64) (Candidate, error) {
	var c Candidate
	err := q.QueryRowContext(ctx, `SELECT `+candidateCols+` FROM candidates WHERE id = ?`, id).Scan(
		&c.ID, &c.Platform, &c.VODID, &c.StartSeconds, &c.EndSeconds, &c.Score, &c.Signal, &c.Status,
		timeCol{&c.CreatedAt}, timeCol{&c.UpdatedAt})
	return c, wrap("get candidate", err)
}

// UpdateCandidate stores the score, signal and status of c. The VOD and the
// window of a candidate never change.
func UpdateCandidate(ctx context.Context, q Querier, c Candidate) error {
	return update(ctx, q, "update candidate", `
		UPDATE candidates SET score = ?, signal = ?, status = ?, updated_at = ? WHERE id = ?`,
		c.Score, c.Signal, c.Status, timeArg(time.Now()), c.ID)
}

// ---- jobs ----

const jobCols = `id, platform, candidate_id, status, attempts, clipper, lease_expires_at, last_heartbeat_at,
	last_error, created_at, updated_at`

func getJob(ctx context.Context, q Querier, op, where string, args ...any) (Job, error) {
	var j Job
	err := q.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE `+where, args...).Scan(
		&j.ID, &j.Platform, &j.CandidateID, &j.Status, &j.Attempts, &j.Clipper,
		optTimeCol{&j.LeaseExpiresAt}, optTimeCol{&j.LastHeartbeatAt}, &j.LastError,
		timeCol{&j.CreatedAt}, timeCol{&j.UpdatedAt})
	return j, wrap(op, err)
}

// InsertJob stores a job and returns the stored row. An empty Status becomes
// JobQueued. A second job for the same candidate is ErrDuplicate. The
// platform must be the candidate's.
func InsertJob(ctx context.Context, q Querier, j Job) (Job, error) {
	if j.Status == "" {
		j.Status = JobQueued
	}
	j.LeaseExpiresAt, j.LastHeartbeatAt = stored(j.LeaseExpiresAt), stored(j.LastHeartbeatAt)
	j.CreatedAt, j.UpdatedAt = stamps(j.CreatedAt)
	id, err := insert(ctx, q, "insert job", `
		INSERT INTO jobs (platform, candidate_id, status, attempts, clipper, lease_expires_at, last_heartbeat_at,
			last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.Platform, j.CandidateID, j.Status, j.Attempts, j.Clipper,
		optTimeArg(j.LeaseExpiresAt), optTimeArg(j.LastHeartbeatAt), j.LastError,
		timeArg(j.CreatedAt), timeArg(j.UpdatedAt))
	j.ID = id
	return j, err
}

// GetJob returns the job with the given row id.
func GetJob(ctx context.Context, q Querier, id int64) (Job, error) {
	return getJob(ctx, q, "get job", `id = ?`, id)
}

// GetJobByCandidate returns the job of a candidate.
func GetJobByCandidate(ctx context.Context, q Querier, candidateID int64) (Job, error) {
	return getJob(ctx, q, "get job", `candidate_id = ?`, candidateID)
}

// UpdateJob stores the status, attempt count, clipper, lease, heartbeat time
// and last error of j. LeaseExpiresAt must be set when the status is
// JobLeased and zero otherwise.
func UpdateJob(ctx context.Context, q Querier, j Job) error {
	return update(ctx, q, "update job", `
		UPDATE jobs SET status = ?, attempts = ?, clipper = ?, lease_expires_at = ?, last_heartbeat_at = ?,
			last_error = ?, updated_at = ?
		WHERE id = ?`,
		j.Status, j.Attempts, j.Clipper, optTimeArg(j.LeaseExpiresAt), optTimeArg(j.LastHeartbeatAt),
		j.LastError, timeArg(time.Now()), j.ID)
}

// ---- clips ----

const clipCols = `id, platform, job_id, file_path, sha256, size_bytes, duration_seconds, width, height,
	transcript_json, clipper, review_status, reviewed_at, telegram_chat_id, telegram_message_id,
	file_removed_at, created_at, updated_at`

func getClip(ctx context.Context, q Querier, op, where string, args ...any) (Clip, error) {
	var c Clip
	err := q.QueryRowContext(ctx, `SELECT `+clipCols+` FROM clips WHERE `+where, args...).Scan(
		&c.ID, &c.Platform, &c.JobID, &c.FilePath, &c.SHA256, &c.SizeBytes, &c.DurationSeconds,
		&c.Width, &c.Height, &c.TranscriptJSON, &c.Clipper, &c.ReviewStatus, optTimeCol{&c.ReviewedAt},
		optIDCol{&c.TelegramChatID}, optIDCol{&c.TelegramMessageID}, optTimeCol{&c.FileRemovedAt},
		timeCol{&c.CreatedAt}, timeCol{&c.UpdatedAt})
	return c, wrap(op, err)
}

// InsertClip stores the result of a job and returns the stored row. An empty
// ReviewStatus becomes ReviewPending. A second clip for the same job is
// ErrDuplicate: the first result wins. The platform must be the job's.
func InsertClip(ctx context.Context, q Querier, c Clip) (Clip, error) {
	if c.ReviewStatus == "" {
		c.ReviewStatus = ReviewPending
	}
	c.ReviewedAt, c.FileRemovedAt = stored(c.ReviewedAt), stored(c.FileRemovedAt)
	c.CreatedAt, c.UpdatedAt = stamps(c.CreatedAt)
	id, err := insert(ctx, q, "insert clip", `
		INSERT INTO clips (platform, job_id, file_path, sha256, size_bytes, duration_seconds, width, height,
			transcript_json, clipper, review_status, reviewed_at, telegram_chat_id, telegram_message_id,
			file_removed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Platform, c.JobID, c.FilePath, c.SHA256, c.SizeBytes, c.DurationSeconds, c.Width, c.Height,
		c.TranscriptJSON, c.Clipper, c.ReviewStatus, optTimeArg(c.ReviewedAt),
		optIDArg(c.TelegramChatID), optIDArg(c.TelegramMessageID), optTimeArg(c.FileRemovedAt),
		timeArg(c.CreatedAt), timeArg(c.UpdatedAt))
	c.ID = id
	return c, err
}

// GetClip returns the clip with the given row id.
func GetClip(ctx context.Context, q Querier, id int64) (Clip, error) {
	return getClip(ctx, q, "get clip", `id = ?`, id)
}

// GetClipByJob returns the accepted result of a job.
func GetClipByJob(ctx context.Context, q Querier, jobID int64) (Clip, error) {
	return getClip(ctx, q, "get clip", `job_id = ?`, jobID)
}

// UpdateClip stores the file path, the review fields, the Telegram message
// and the file-removed time of c. The job and what describes the rendered
// file (checksum, size, duration, resolution, transcript, clipper) never
// change. ReviewedAt must be zero when the status is ReviewPending and set
// otherwise.
func UpdateClip(ctx context.Context, q Querier, c Clip) error {
	return update(ctx, q, "update clip", `
		UPDATE clips SET file_path = ?, review_status = ?, reviewed_at = ?, telegram_chat_id = ?,
			telegram_message_id = ?, file_removed_at = ?, updated_at = ?
		WHERE id = ?`,
		c.FilePath, c.ReviewStatus, optTimeArg(c.ReviewedAt), optIDArg(c.TelegramChatID),
		optIDArg(c.TelegramMessageID), optTimeArg(c.FileRemovedAt), timeArg(time.Now()), c.ID)
}

// ---- uploads ----

const uploadCols = `id, clip_id, target_platform, status, external_post_id, attempts, last_error,
	next_attempt_at, posted_at, created_at, updated_at`

func getUpload(ctx context.Context, q Querier, op, where string, args ...any) (Upload, error) {
	var u Upload
	err := q.QueryRowContext(ctx, `SELECT `+uploadCols+` FROM uploads WHERE `+where, args...).Scan(
		&u.ID, &u.ClipID, &u.TargetPlatform, &u.Status, &u.ExternalPostID, &u.Attempts, &u.LastError,
		optTimeCol{&u.NextAttemptAt}, optTimeCol{&u.PostedAt},
		timeCol{&u.CreatedAt}, timeCol{&u.UpdatedAt})
	return u, wrap(op, err)
}

// InsertUpload stores an upload and returns the stored row. An empty Status
// becomes UploadPending. A second upload of the same clip to the same target
// is ErrDuplicate.
func InsertUpload(ctx context.Context, q Querier, u Upload) (Upload, error) {
	if u.Status == "" {
		u.Status = UploadPending
	}
	u.NextAttemptAt, u.PostedAt = stored(u.NextAttemptAt), stored(u.PostedAt)
	u.CreatedAt, u.UpdatedAt = stamps(u.CreatedAt)
	id, err := insert(ctx, q, "insert upload", `
		INSERT INTO uploads (clip_id, target_platform, status, external_post_id, attempts, last_error,
			next_attempt_at, posted_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ClipID, u.TargetPlatform, u.Status, u.ExternalPostID, u.Attempts, u.LastError,
		optTimeArg(u.NextAttemptAt), optTimeArg(u.PostedAt),
		timeArg(u.CreatedAt), timeArg(u.UpdatedAt))
	u.ID = id
	return u, err
}

// GetUpload returns the upload with the given row id.
func GetUpload(ctx context.Context, q Querier, id int64) (Upload, error) {
	return getUpload(ctx, q, "get upload", `id = ?`, id)
}

// GetUploadByTarget returns the upload of a clip to one target.
func GetUploadByTarget(ctx context.Context, q Querier, clipID int64, targetPlatform string) (Upload, error) {
	return getUpload(ctx, q, "get upload", `clip_id = ? AND target_platform = ?`, clipID, targetPlatform)
}

// UpdateUpload stores the status, post id, attempt count, last error, next
// attempt time and posted time of u. PostedAt must be set when the status is
// UploadDone and zero otherwise.
func UpdateUpload(ctx context.Context, q Querier, u Upload) error {
	return update(ctx, q, "update upload", `
		UPDATE uploads SET status = ?, external_post_id = ?, attempts = ?, last_error = ?,
			next_attempt_at = ?, posted_at = ?, updated_at = ?
		WHERE id = ?`,
		u.Status, u.ExternalPostID, u.Attempts, u.LastError,
		optTimeArg(u.NextAttemptAt), optTimeArg(u.PostedAt), timeArg(time.Now()), u.ID)
}
