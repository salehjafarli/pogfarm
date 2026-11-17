package intake

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// Errors of Complete, matched with errors.Is. Each stands for one error code
// of the contract; see Code.
var (
	// ErrInvalid: the result is malformed (CodeInvalidRequest): a missing
	// or misplaced part, malformed JSON, an invalid field, an upload that
	// ended early, or a clip whose size differs from size_bytes although
	// its checksum matches.
	ErrInvalid = errors.New("intake: invalid result")
	// ErrTooLarge: a part of the upload exceeds its limit
	// (CodePayloadTooLarge).
	ErrTooLarge = errors.New("intake: payload too large")
	// ErrChecksumMismatch: the clip received does not match the SHA-256 in
	// the metadata (CodeChecksumMismatch).
	ErrChecksumMismatch = errors.New("intake: checksum mismatch")
	// ErrUnknownJob is queue.ErrUnknownJob: no job has the given id
	// (CodeUnknownJob).
	ErrUnknownJob = queue.ErrUnknownJob
)

// Code returns the contract's error code for an error of Complete or of the
// handler: CodeInternal for anything that is not one of the errors above.
func Code(err error) api.ErrorCode {
	switch {
	case errors.Is(err, ErrInvalid):
		return api.CodeInvalidRequest
	case errors.Is(err, ErrTooLarge):
		return api.CodePayloadTooLarge
	case errors.Is(err, ErrChecksumMismatch):
		return api.CodeChecksumMismatch
	case errors.Is(err, ErrUnknownJob):
		return api.CodeUnknownJob
	default:
		return api.CodeInternal
	}
}

// Fixed limits of the two small parts of an upload. The limit of the clip
// part comes from Options.MaxClipBytes.
const (
	// MaxMetadataBytes caps the metadata part.
	MaxMetadataBytes = 64 << 10
	// MaxTranscriptBytes caps the transcript part, which is a few kilobytes
	// for a clip of a minute.
	MaxTranscriptBytes = 4 << 20
)

// Options configure an Intake.
type Options struct {
	// DataDir is pogfarm's data directory. Clip files are stored below it
	// and clips.file_path is relative to it. Required.
	DataDir string
	// MaxClipBytes is the largest clip accepted, in bytes. Required, and
	// positive.
	MaxClipBytes int64
	// Now is the clock. Default time.Now.
	Now func() time.Time
	// Logger receives internal errors of the handler. Default: discarded.
	Logger *slog.Logger
}

// Intake stores the results of render jobs. It is safe for concurrent use.
type Intake struct {
	store   *store.Store
	dataDir string
	maxClip int64
	now     func() time.Time
	logger  *slog.Logger

	// finalize serialises the step from "is there a result already?" to the
	// commit of the new one, so two results for a job can never both be
	// moved into place. It is held for a rename and one short transaction;
	// receiving and hashing uploads runs in parallel outside it.
	finalize sync.Mutex

	// beforeCommit, when set by a test, runs after the clip is in place and
	// before the transaction that records it.
	beforeCommit func()
}

// New returns the intake for st, creating the clip and temp directories
// below opts.DataDir when they do not exist.
func New(st *store.Store, opts Options) (*Intake, error) {
	if st == nil {
		return nil, errors.New("intake: store is nil")
	}
	if opts.DataDir == "" {
		return nil, errors.New("intake: data directory is empty")
	}
	if opts.MaxClipBytes <= 0 {
		return nil, fmt.Errorf("intake: max clip size %d is not positive", opts.MaxClipBytes)
	}
	in := &Intake{
		store:   st,
		dataDir: opts.DataDir,
		maxClip: opts.MaxClipBytes,
		now:     opts.Now,
		logger:  opts.Logger,
	}
	if in.now == nil {
		in.now = time.Now
	}
	if in.logger == nil {
		in.logger = slog.New(slog.DiscardHandler)
	}
	for _, dir := range []string{ClipsDir, TempDir} {
		if err := os.MkdirAll(filepath.Join(in.dataDir, dir), 0o750); err != nil {
			return nil, fmt.Errorf("intake: %w", err)
		}
	}
	return in, nil
}

// MaxClipBytes returns the largest clip accepted.
func (in *Intake) MaxClipBytes() int64 { return in.maxClip }

// Complete stores the result of job jobID: the clip read from clip, which is
// streamed to disk and never held in memory, with its metadata and
// transcript.
//
// It returns api.ResultAccepted when this result became the job's result,
// and api.ResultAlreadyCompleted when the job already had one; this result
// is then discarded. Errors match ErrInvalid, ErrTooLarge,
// ErrChecksumMismatch or ErrUnknownJob; any other error is internal. On
// every error and on ResultAlreadyCompleted nothing is stored and no file
// is left behind.
//
// A read error from clip counts as an upload that ended early (ErrInvalid)
// unless it already matches one of the errors above.
func (in *Intake) Complete(ctx context.Context, jobID int64, meta api.CompleteMetadata, transcript api.Transcript, clip io.Reader) (api.CompleteResult, error) {
	if err := in.checkMetadata(meta); err != nil {
		return "", err
	}
	transcriptJSON, err := checkTranscript(transcript)
	if err != nil {
		return "", err
	}

	tmp, err := in.receive(clip, meta)
	if err != nil {
		return "", err
	}
	return in.finalizeResult(ctx, jobID, meta, transcriptJSON, tmp)
}

// checkMetadata validates the metadata part.
func (in *Intake) checkMetadata(m api.CompleteMetadata) error {
	switch {
	case m.Clipper == "":
		return fmt.Errorf("%w: metadata: clipper is empty", ErrInvalid)
	case !isSHA256Hex(m.SHA256):
		return fmt.Errorf("%w: metadata: sha256 is not 64 lowercase hex digits", ErrInvalid)
	case m.SizeBytes <= 0:
		return fmt.Errorf("%w: metadata: size_bytes %d is not positive", ErrInvalid, m.SizeBytes)
	case m.SizeBytes > in.maxClip:
		return fmt.Errorf("%w: clip of %d bytes exceeds the limit of %d bytes", ErrTooLarge, m.SizeBytes, in.maxClip)
	case !(m.DurationSeconds > 0) || math.IsInf(m.DurationSeconds, 0):
		return fmt.Errorf("%w: metadata: duration_seconds %v is not positive", ErrInvalid, m.DurationSeconds)
	case m.Width <= 0 || m.Height <= 0:
		return fmt.Errorf("%w: metadata: resolution %dx%d is not positive", ErrInvalid, m.Width, m.Height)
	}
	return nil
}

func isSHA256Hex(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// checkTranscript validates the transcript part and returns it as it is
// stored.
func checkTranscript(t api.Transcript) (string, error) {
	if t.Words == nil {
		return "", fmt.Errorf("%w: transcript: words is missing or null", ErrInvalid)
	}
	for i, w := range t.Words {
		if math.IsNaN(w.StartSeconds) || math.IsInf(w.StartSeconds, 0) || math.IsInf(w.EndSeconds, 0) ||
			w.StartSeconds < 0 || !(w.EndSeconds >= w.StartSeconds) {
			return "", fmt.Errorf("%w: transcript: word %d has times %v to %v", ErrInvalid, i, w.StartSeconds, w.EndSeconds)
		}
	}
	data, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("%w: transcript: %v", ErrInvalid, err)
	}
	return string(data), nil
}

// sourceReader remembers the error its reader returned, so a failed copy
// can tell a broken upload from a failed write.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		s.err = err
	}
	return n, err
}

// receive streams clip into a new file in TempDir while hashing it, checks
// it against meta and syncs it. It returns the file's path; on error the
// file is already removed.
func (in *Intake) receive(clip io.Reader, meta api.CompleteMetadata) (_ string, err error) {
	f, err := os.CreateTemp(filepath.Join(in.dataDir, TempDir), "upload-*.mp4")
	if err != nil {
		return "", fmt.Errorf("intake: create temp file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if f != nil {
			_ = f.Close()
		}
		if err != nil {
			removeFile(in.logger, tmp)
		}
	}()

	h := sha256.New()
	// One byte more than the limit tells an oversize clip from one that is
	// exactly at the limit without reading the rest.
	src := &sourceReader{r: io.LimitReader(clip, in.maxClip+1)}
	n, err := io.Copy(io.MultiWriter(f, h), src)
	if err != nil {
		if src.err == nil {
			return "", fmt.Errorf("intake: write temp file: %w", err)
		}
		if errors.Is(src.err, ErrInvalid) || errors.Is(src.err, ErrTooLarge) {
			return "", src.err
		}
		return "", fmt.Errorf("%w: clip: upload ended early: %v", ErrInvalid, src.err)
	}
	if n > in.maxClip {
		return "", fmt.Errorf("%w: clip exceeds the limit of %d bytes", ErrTooLarge, in.maxClip)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != meta.SHA256 {
		return "", fmt.Errorf("%w: received %d bytes with sha256 %s, metadata says %s", ErrChecksumMismatch, n, sum, meta.SHA256)
	}
	if n != meta.SizeBytes {
		return "", fmt.Errorf("%w: clip has %d bytes, metadata says %d", ErrInvalid, n, meta.SizeBytes)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("intake: sync temp file: %w", err)
	}
	cerr := f.Close()
	f = nil
	if cerr != nil {
		return "", fmt.Errorf("intake: close temp file: %w", cerr)
	}
	return tmp, nil
}

// errHasResult ends the transaction of finalizeResult when the job turns
// out to have a result already.
var errHasResult = errors.New("intake: job already has a result")

// finalizeResult makes the verified file at tmp the job's result unless the
// job already has one. The file is renamed into place before the
// transaction that records it, and removed again if that transaction does
// not commit. tmp is removed unless it was moved into place.
func (in *Intake) finalizeResult(ctx context.Context, jobID int64, meta api.CompleteMetadata, transcriptJSON, tmp string) (api.CompleteResult, error) {
	moved := false
	defer func() {
		if !moved {
			removeFile(in.logger, tmp)
		}
	}()

	in.finalize.Lock()
	defer in.finalize.Unlock()

	// Every earlier result has committed (the lock is held across the
	// commit), so this read sees it.
	job, err := store.GetJob(ctx, in.store.Reader(), jobID)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("%w: %d", ErrUnknownJob, jobID)
	}
	if err != nil {
		return "", fmt.Errorf("intake: job %d: %w", jobID, err)
	}
	if job.Status == store.JobDone {
		return api.ResultAlreadyCompleted, nil
	}
	switch _, err := store.GetClipByJob(ctx, in.store.Reader(), jobID); {
	case err == nil:
		return api.ResultAlreadyCompleted, nil
	case !errors.Is(err, store.ErrNotFound):
		return "", fmt.Errorf("intake: job %d: %w", jobID, err)
	}

	// No row refers to the destination, so a file already there is an
	// orphan left by a crash between rename and commit; it is replaced.
	now := in.now().UTC().Truncate(time.Millisecond)
	rel := ClipPath(jobID, now)
	dst := filepath.Join(in.dataDir, filepath.FromSlash(rel))
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("intake: job %d: %w", jobID, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", fmt.Errorf("intake: job %d: move clip into place: %w", jobID, err)
	}
	moved = true
	if err := syncDir(dir); err != nil {
		removeFile(in.logger, dst)
		return "", fmt.Errorf("intake: job %d: %w", jobID, err)
	}

	if in.beforeCommit != nil {
		in.beforeCommit()
	}
	err = in.store.WriteTx(ctx, func(tx *sql.Tx) error {
		job, err := store.GetJob(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if job.Status == store.JobDone {
			return errHasResult
		}
		_, err = store.InsertClip(ctx, tx, store.Clip{
			Platform:        job.Platform,
			JobID:           jobID,
			FilePath:        rel,
			SHA256:          meta.SHA256,
			SizeBytes:       meta.SizeBytes,
			DurationSeconds: meta.DurationSeconds,
			Width:           meta.Width,
			Height:          meta.Height,
			TranscriptJSON:  transcriptJSON,
			Clipper:         meta.Clipper,
			ReviewStatus:    store.ReviewPending,
			CreatedAt:       now,
		})
		if errors.Is(err, store.ErrDuplicate) {
			return errHasResult
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE jobs SET status = ?, lease_expires_at = NULL, updated_at = ? WHERE id = ?`,
			store.JobDone, store.FormatTime(now), jobID)
		return err
	})
	if err != nil {
		// Nothing refers to the file. Under the lock no other result can
		// have been moved to the same path, so it is this upload's.
		removeFile(in.logger, dst)
		if errors.Is(err, errHasResult) {
			return api.ResultAlreadyCompleted, nil
		}
		return "", fmt.Errorf("intake: job %d: record result: %w", jobID, err)
	}
	return api.ResultAccepted, nil
}

// syncDir makes a rename in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}

// removeFile deletes a file nothing refers to. A failure leaves an orphan
// (or a leftover temp file) for the file cleanup and is only logged.
func removeFile(logger *slog.Logger, path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Error("intake: remove unrecorded file", "path", path, "err", err)
	}
}
