package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LocalOutputBase is the name, without extension, of the file the local-file
// downloader writes. A trimmed range is LocalOutputBase + ".mp4"; a file
// copied whole keeps its own extension.
const LocalOutputBase = "source"

// Local is the Downloader for a file already on disk (clipper render
// --input). It needs no network and no TwitchDownloaderCLI: without a range
// it copies the file into the work directory, with one it cuts the range out
// with ffmpeg.
type Local struct {
	tools tools
}

// Download implements Downloader. It reads req.Input, req.Start and
// req.End.
func (l *Local) Download(ctx context.Context, req Request, workDir string) (Result, error) {
	if !req.HasRange() && req.Start != 0 {
		return Result{}, fmt.Errorf("download: start %s given without an end: %w", req.Start, ErrInvalidRequest)
	}
	if req.HasRange() {
		if err := validateRange(req); err != nil {
			return Result{}, err
		}
	}
	fi, err := os.Stat(req.Input)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Result{}, fmt.Errorf("download: input %s does not exist: %w", req.Input, ErrInputMissing)
	case err != nil:
		return Result{}, fmt.Errorf("download: input %s: %w", req.Input, err)
	case !fi.Mode().IsRegular():
		return Result{}, fmt.Errorf("download: input %s is not a regular file: %w", req.Input, ErrInputMissing)
	}

	if !req.HasRange() {
		out := filepath.Join(workDir, LocalOutputBase+strings.ToLower(filepath.Ext(req.Input)))
		if err := copyFile(ctx, req.Input, out); err != nil {
			return Result{}, err
		}
		return l.tools.verify(ctx, out, 0)
	}

	// Check the range against the input first, so a range past its end is
	// reported as such rather than as a short result.
	in, err := l.tools.probe(ctx, req.Input)
	if err != nil {
		return Result{}, err
	}
	if in.Duration > 0 && req.End > in.Duration+DurationTolerance {
		return Result{}, fmt.Errorf("download: end %s is past the end of %s (%s): %w",
			req.End, req.Input, in.Duration, ErrInvalidRequest)
	}
	out := filepath.Join(workDir, LocalOutputBase+".mp4")
	if err := l.tools.trim(ctx, req.Input, out, req.Start, req.Length()); err != nil {
		return Result{}, err
	}
	return l.tools.verify(ctx, out, req.Length())
}

// copyFile copies src to dst, stopping when ctx is done.
func copyFile(ctx context.Context, src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("download: copy: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("download: copy: %w", err)
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("download: copy: %w", cerr)
		}
	}()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("download: copy: %w", err)
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return fmt.Errorf("download: copy: %w", werr)
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return fmt.Errorf("download: copy: %w", rerr)
		}
	}
}
