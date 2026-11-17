package intake

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// DefaultOrphanGrace is how old a file under ClipsDir that no clip row
// refers to must be before the cleanup deletes it, unless
// CleanupOptions.OrphanGrace says otherwise.
const DefaultOrphanGrace = time.Hour

// CleanupOptions configure a Cleaner.
type CleanupOptions struct {
	// RejectedRetention is how long the file of a rejected clip is kept
	// after the decision. Required, and positive.
	RejectedRetention time.Duration
	// PostedRetention is how long the file of a clip that has been posted
	// to every target is kept after its last post. Required, and positive.
	PostedRetention time.Duration
	// OrphanGrace is how old, by modification time, a file that no clip row
	// refers to must be (at least) before it is deleted. Zero means
	// DefaultOrphanGrace; negative is an error.
	OrphanGrace time.Duration
	// Now is the clock. Default time.Now.
	Now func() time.Time
	// Logger receives what a sweep removed and the problems it skipped
	// over. Default: discarded.
	Logger *slog.Logger
}

// Cleaner deletes clip files that are no longer needed and empties TempDir.
// It belongs to one Intake and is safe for concurrent use.
//
// A sweep (Sweep) does two things:
//
//   - It deletes the file of every clip whose retention is over: a rejected
//     clip RejectedRetention after it was rejected, and an approved clip
//     PostedRetention after its last post, once it has at least one upload
//     row and every upload row is done. A clip that is pending review, or
//     approved with no upload rows, or with an upload that is pending,
//     uploading or failed, is never touched. For each such clip it first
//     sets clips.file_removed_at, in a short write transaction whose UPDATE
//     checks the conditions again, and deletes the file only after that
//     commits: the database may for a moment say a file is gone that is
//     still there, never the opposite. A file left behind because its
//     deletion failed is no longer referred to and goes as an orphan.
//
//   - It deletes orphans: regular files under ClipsDir that no clip row with
//     file_removed_at unset refers to and that are older than OrphanGrace.
//     Each is deleted while holding the lock under which Intake moves a
//     result into place and commits it, after checking again that no row
//     refers to it, so a result between its rename and its commit is never
//     mistaken for an orphan. The grace period is a second guard.
//
// Paths are only ever resolved inside ClipsDir: a file_path that is not a
// clean relative path below ClipsDir is skipped and logged, and every
// removal goes through an os.Root, so neither ".." nor a symbolic link can
// reach outside it. Directories are never removed.
type Cleaner struct {
	in       *Intake
	rejected time.Duration
	posted   time.Duration
	grace    time.Duration
	now      func() time.Time
	logger   *slog.Logger
}

// NewCleaner returns the cleaner of the files in's results are stored in.
func (in *Intake) NewCleaner(opts CleanupOptions) (*Cleaner, error) {
	if opts.RejectedRetention <= 0 {
		return nil, fmt.Errorf("intake: rejected retention %s is not positive", opts.RejectedRetention)
	}
	if opts.PostedRetention <= 0 {
		return nil, fmt.Errorf("intake: posted retention %s is not positive", opts.PostedRetention)
	}
	if opts.OrphanGrace < 0 {
		return nil, fmt.Errorf("intake: orphan grace %s is negative", opts.OrphanGrace)
	}
	c := &Cleaner{
		in:       in,
		rejected: opts.RejectedRetention,
		posted:   opts.PostedRetention,
		grace:    opts.OrphanGrace,
		now:      opts.Now,
		logger:   opts.Logger,
	}
	if c.grace == 0 {
		c.grace = DefaultOrphanGrace
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.logger == nil {
		c.logger = slog.New(slog.DiscardHandler)
	}
	return c, nil
}

// EmptyTemp deletes everything in TempDir, which holds only uploads that
// were in flight when pogfarm stopped. It returns how many entries it
// removed and how many bytes of regular files that freed.
//
// Call it once at startup, before the job API serves uploads: it would
// delete an upload in progress.
func (c *Cleaner) EmptyTemp() (removed int, freed int64, err error) {
	dir := filepath.Join(c.in.dataDir, TempDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return 0, 0, fmt.Errorf("intake: empty temp directory: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, fmt.Errorf("intake: empty temp directory: %w", err)
	}
	var errs []error
	for _, e := range entries {
		var size int64
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			size = info.Size()
		}
		// RemoveAll removes a symbolic link, not what it points to.
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
		freed += size
	}
	if err := errors.Join(errs...); err != nil {
		return removed, freed, fmt.Errorf("intake: empty temp directory: %w", err)
	}
	if removed > 0 {
		c.logger.Info("intake: emptied temp directory", "entries", removed, "freed_bytes", freed)
	}
	return removed, freed, nil
}

// SweepResult says what one sweep did.
type SweepResult struct {
	// Rejected and Posted count the clips whose file was marked removed,
	// by why. Missing counts those among them whose file was already gone.
	Rejected int
	Posted   int
	Missing  int
	// Orphans counts the orphan files deleted.
	Orphans int
	// FreedBytes is the size of all files deleted.
	FreedBytes int64
	// Skipped counts the clips due for cleanup whose file_path is not a
	// path below ClipsDir; they are left alone.
	Skipped int
}

// Sweep makes one cleanup pass; see Cleaner. Run calls it on a timer.
//
// A database error ends the pass and is returned with what was done so far.
// A file that cannot be deleted is skipped; the pass goes on and returns
// those errors joined at the end. Sweep is idempotent: a second pass right
// after the first finds nothing to do.
func (c *Cleaner) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	clipsDir := filepath.Join(c.in.dataDir, ClipsDir)
	if err := os.MkdirAll(clipsDir, 0o750); err != nil {
		return res, fmt.Errorf("intake: cleanup: %w", err)
	}
	root, err := os.OpenRoot(clipsDir)
	if err != nil {
		return res, fmt.Errorf("intake: cleanup: %w", err)
	}
	defer root.Close()

	now := c.now().UTC()
	var fileErrs []error
	if err := c.sweepClips(ctx, root, now, &res, &fileErrs); err != nil {
		return res, err
	}
	if err := c.sweepOrphans(ctx, root, now, &res, &fileErrs); err != nil {
		return res, err
	}
	return res, errors.Join(fileErrs...)
}

// dueWhere selects the clips whose file is due for deletion. Its arguments
// are the latest decision time of a rejected clip and the latest last-post
// time of a posted one. clips_cleanup serves the first branch and the
// (clip_id, target_platform) unique index of uploads the subqueries.
const dueWhere = `
	file_removed_at IS NULL AND (
		(review_status = 'rejected' AND reviewed_at <= ?)
		OR (review_status = 'approved'
			AND EXISTS (SELECT 1 FROM uploads u WHERE u.clip_id = clips.id)
			AND NOT EXISTS (SELECT 1 FROM uploads u WHERE u.clip_id = clips.id AND u.status <> 'done')
			AND (SELECT max(u.posted_at) FROM uploads u WHERE u.clip_id = clips.id) <= ?))`

type dueClip struct {
	id     int64
	path   string
	status store.ReviewStatus
}

// sweepClips deletes the files of the clips whose retention is over.
func (c *Cleaner) sweepClips(ctx context.Context, root *os.Root, now time.Time, res *SweepResult, fileErrs *[]error) error {
	rejectedBefore := store.FormatTime(now.Add(-c.rejected))
	postedBefore := store.FormatTime(now.Add(-c.posted))
	due, err := c.dueClips(ctx, rejectedBefore, postedBefore)
	if err != nil {
		return err
	}
	stamp := store.FormatTime(now)
	for _, d := range due {
		if err := ctx.Err(); err != nil {
			return err
		}
		name, ok := clipFileName(d.path)
		if !ok {
			res.Skipped++
			c.logger.Warn("intake: cleanup: clip file path is not below the clips directory, left alone",
				"clip", d.id, "file_path", d.path)
			continue
		}
		// The UPDATE checks the conditions again: a clip whose state
		// changed since it was selected (an undone rejection, say) is left
		// alone.
		r, err := c.in.store.Writer().ExecContext(ctx,
			`UPDATE clips SET file_removed_at = ?, updated_at = ? WHERE id = ? AND `+dueWhere,
			stamp, stamp, d.id, rejectedBefore, postedBefore)
		if err != nil {
			return fmt.Errorf("intake: cleanup: mark clip %d file removed: %w", d.id, err)
		}
		if n, err := r.RowsAffected(); err != nil {
			return fmt.Errorf("intake: cleanup: mark clip %d file removed: %w", d.id, err)
		} else if n == 0 {
			continue
		}
		if d.status == store.ReviewRejected {
			res.Rejected++
		} else {
			res.Posted++
		}
		size, found, err := removeIn(root, name)
		switch {
		case err != nil:
			*fileErrs = append(*fileErrs, fmt.Errorf("intake: cleanup: clip %d: %w", d.id, err))
		case !found:
			res.Missing++
			c.logger.Warn("intake: cleanup: clip file was already gone", "clip", d.id, "file_path", d.path)
		default:
			res.FreedBytes += size
		}
	}
	return nil
}

// dueClips returns the clips whose file is due for deletion, on a read
// connection.
func (c *Cleaner) dueClips(ctx context.Context, rejectedBefore, postedBefore string) ([]dueClip, error) {
	rows, err := c.in.store.Reader().QueryContext(ctx,
		`SELECT id, file_path, review_status FROM clips WHERE `+dueWhere+` ORDER BY id`,
		rejectedBefore, postedBefore)
	if err != nil {
		return nil, fmt.Errorf("intake: cleanup: find clips: %w", err)
	}
	defer rows.Close()
	var due []dueClip
	for rows.Next() {
		var d dueClip
		if err := rows.Scan(&d.id, &d.path, &d.status); err != nil {
			return nil, fmt.Errorf("intake: cleanup: find clips: %w", err)
		}
		due = append(due, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("intake: cleanup: find clips: %w", err)
	}
	return due, nil
}

// sweepOrphans deletes the old files under ClipsDir that no row refers to.
func (c *Cleaner) sweepOrphans(ctx context.Context, root *os.Root, now time.Time, res *SweepResult, fileErrs *[]error) error {
	cutoff := now.Add(-c.grace)

	// The walk comes first: a result committed after it is not among the
	// files found, and one committed before the referenced paths are read
	// below is among those paths.
	var old []string
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			*fileErrs = append(*fileErrs, fmt.Errorf("intake: cleanup: %w", err))
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				*fileErrs = append(*fileErrs, fmt.Errorf("intake: cleanup: %w", err))
			}
			return nil
		}
		if !info.ModTime().After(cutoff) {
			old = append(old, name)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("intake: cleanup: walk clips directory: %w", err)
	}
	if len(old) == 0 {
		return nil
	}
	referenced, err := c.referencedPaths(ctx)
	if err != nil {
		return err
	}
	for _, name := range old {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := path.Join(ClipsDir, name)
		if referenced[rel] {
			continue
		}
		size, removed, err := c.removeOrphan(ctx, root, filepath.FromSlash(name), rel, cutoff)
		if err != nil {
			*fileErrs = append(*fileErrs, err)
			continue
		}
		if removed {
			res.Orphans++
			res.FreedBytes += size
			c.logger.Info("intake: cleanup: removed orphan file", "file_path", rel, "bytes", size)
		}
	}
	return nil
}

// referencedPaths returns the file paths of the clips whose file has not
// been removed.
func (c *Cleaner) referencedPaths(ctx context.Context) (map[string]bool, error) {
	rows, err := c.in.store.Reader().QueryContext(ctx,
		`SELECT file_path FROM clips WHERE file_removed_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("intake: cleanup: read clip paths: %w", err)
	}
	defer rows.Close()
	paths := make(map[string]bool)
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("intake: cleanup: read clip paths: %w", err)
		}
		paths[p] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("intake: cleanup: read clip paths: %w", err)
	}
	return paths, nil
}

// removeOrphan deletes the file name (rel from the data directory) if,
// under Intake's finalize lock, no row refers to it and it is still an old
// regular file. While the lock is held no result is between its rename and
// its commit, so a file no row refers to is not about to be recorded.
func (c *Cleaner) removeOrphan(ctx context.Context, root *os.Root, name, rel string, cutoff time.Time) (int64, bool, error) {
	c.in.finalize.Lock()
	defer c.in.finalize.Unlock()

	var referenced bool
	err := c.in.store.Reader().QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM clips WHERE file_path = ? AND file_removed_at IS NULL)`, rel).Scan(&referenced)
	if err != nil {
		return 0, false, fmt.Errorf("intake: cleanup: orphan %s: %w", rel, err)
	}
	if referenced {
		return 0, false, nil
	}
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("intake: cleanup: orphan %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
		return 0, false, nil
	}
	size, found, err := removeIn(root, name)
	if err != nil {
		return 0, false, fmt.Errorf("intake: cleanup: orphan %s: %w", rel, err)
	}
	return size, found, nil
}

// removeIn deletes the file name inside root and returns its size. found is
// false when there was no such file.
func removeIn(root *os.Root, name string) (size int64, found bool, err error) {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if err := root.Remove(name); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if info.Mode().IsRegular() {
		size = info.Size()
	}
	return size, true, nil
}

// clipFileName returns the name inside ClipsDir of a stored file_path, in
// the form os.Root takes. ok is false unless p is a clean, relative,
// forward-slash path strictly below ClipsDir, as ClipPath builds them.
func clipFileName(p string) (name string, ok bool) {
	if p == "" || p != path.Clean(p) || strings.ContainsAny(p, "\\\x00") {
		return "", false
	}
	rest, ok := strings.CutPrefix(p, ClipsDir+"/")
	if !ok || rest == "" {
		return "", false
	}
	name = filepath.FromSlash(rest)
	if !filepath.IsLocal(name) {
		return "", false
	}
	return name, true
}

// Run sweeps until ctx is cancelled: once at the start and then every
// interval. It returns nil once ctx is cancelled, promptly, and an error
// only when interval is not positive. It does not empty TempDir; call
// EmptyTemp before the job API starts.
//
// What each pass removed and how much space it freed is logged; a pass that
// fails is logged and the next tick tries again. Run it in a goroutine of
// its own, beside the job API.
func (c *Cleaner) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("intake: cleanup interval %s is not positive", interval)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	c.run(ctx, ticker.C)
	return nil
}

// run is the loop of Run with the timer passed in, so tests can drive it.
func (c *Cleaner) run(ctx context.Context, tick <-chan time.Time) {
	for {
		if ctx.Err() != nil {
			return
		}
		res, err := c.Sweep(ctx)
		if res != (SweepResult{}) {
			c.logger.Info("intake: cleanup pass",
				"rejected", res.Rejected, "posted", res.Posted, "missing", res.Missing,
				"orphans", res.Orphans, "skipped", res.Skipped, "freed_bytes", res.FreedBytes)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.logger.Error("intake: cleanup pass failed, will retry", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
	}
}
