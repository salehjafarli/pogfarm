package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/detect"
	"github.com/salehjafarli/pogfarm/internal/clipper/render"
	"github.com/salehjafarli/pogfarm/internal/config"
)

// runRender is clipper render: it renders one clip into a temporary work
// directory, moves the clip and its transcript to the output path, prints
// the summary and deletes the work directory unless --keep-work was given.
func runRender(ctx context.Context, f renderFlags, e env) (err error) {
	req, err := renderRequest(f)
	if err != nil {
		return err
	}
	out := f.output
	if out == "" {
		out = defaultOutputName(req)
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return err
	}
	transcriptOut := transcriptPath(out)
	if req.Input != "" {
		if in, err := filepath.Abs(req.Input); err == nil && (in == out || in == transcriptOut) {
			return fmt.Errorf("the output %s would replace the input", out)
		}
	}
	// Fail now, not after minutes of rendering.
	if fi, err := os.Stat(filepath.Dir(out)); err != nil || !fi.IsDir() {
		return fmt.Errorf("the output directory %s does not exist", filepath.Dir(out))
	}

	cfg, err := config.LoadClipperRender(f.configPath)
	if err != nil {
		return err
	}
	// With face detection on, refuse to start without a working detector.
	if err := detect.Check(ctx, cfg.WebcamDetection); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(e.stderr, nil))
	r, err := render.New(*cfg, render.Options{Logger: logger, Transcriber: e.transcriber})
	if err != nil {
		return err
	}

	workDir, err := os.MkdirTemp("", "clipper-render-")
	if err != nil {
		return err
	}
	defer func() {
		if f.keepWork {
			fmt.Fprintf(e.stdout, "%-14s%s\n", "work dir", workDir)
			return
		}
		err = errors.Join(err, os.RemoveAll(workDir))
	}()

	res, err := r.Render(ctx, req, workDir)
	if err != nil {
		return err
	}
	if err := moveFile(res.Path, out); err != nil {
		return err
	}
	if err := moveFile(res.TranscriptPath, transcriptOut); err != nil {
		return err
	}
	fmt.Fprint(e.stdout, formatSummary(res, out, transcriptOut))
	return nil
}

// renderRequest turns the flags into a render.Request and checks it.
func renderRequest(f renderFlags) (render.Request, error) {
	req := render.Request{Input: f.input}
	if f.vod != "" {
		req.Platform, req.VODID = f.platform, f.vod
	}
	var err error
	if req.Start, err = parseTime("--start", f.start); err != nil {
		return render.Request{}, err
	}
	if req.End, err = parseTime("--end", f.end); err != nil {
		return render.Request{}, err
	}
	if f.end != "" && req.End == 0 {
		return render.Request{}, errors.New("--end must be after the start of the source")
	}
	if f.start != "" && f.end == "" && req.Start != 0 {
		return render.Request{}, errors.New("--start needs --end")
	}
	if err := req.Validate(); err != nil {
		return render.Request{}, err
	}
	return req, nil
}

// parseTime reads a time in the source: Go's duration form ("1h02m10s",
// "62m10s", "45s"), clock form ("1:02:10", "62:10") or plain seconds
// ("3730", "3730.5"). An empty string is zero.
func parseTime(flagName, s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	bad := func() (time.Duration, error) {
		return 0, fmt.Errorf("%s %q is not a time such as 1h02m10s, 1:02:10 or 3730", flagName, s)
	}
	var d time.Duration
	switch {
	case strings.Contains(s, ":"):
		parts := strings.Split(s, ":")
		if len(parts) > 3 {
			return bad()
		}
		var sec float64
		for i, p := range parts {
			last := i == len(parts)-1
			v, err := strconv.ParseFloat(p, 64)
			if err != nil || v < 0 || (!last && v != float64(int64(v))) || (i > 0 && v >= 60) {
				return bad()
			}
			sec = sec*60 + v
		}
		d = time.Duration(sec * float64(time.Second))
	default:
		if sec, err := strconv.ParseFloat(s, 64); err == nil {
			d = time.Duration(sec * float64(time.Second))
		} else if d, err = time.ParseDuration(s); err != nil {
			return bad()
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("%s %q is negative", flagName, s)
	}
	return d.Round(time.Millisecond), nil
}

// defaultOutputName is the clip's file name when -o is not given: the
// source and the range, for example "twitch-2412345678_1h02m10s-1h02m55s.mp4"
// or "stream_full.mp4" for a whole input file.
func defaultOutputName(req render.Request) string {
	src := req.Platform + "-" + req.VODID
	if req.Input != "" {
		base := filepath.Base(req.Input)
		src = strings.TrimSuffix(base, filepath.Ext(base))
	}
	rng := "full"
	if req.End != 0 {
		rng = compactTime(req.Start) + "-" + compactTime(req.End)
	}
	return safeName(src) + "_" + rng + ".mp4"
}

// compactTime writes d for a file name: "1h02m10s", "2m05s", "45s", "10.5s".
func compactTime(d time.Duration) string {
	d = d.Round(time.Millisecond)
	h := d / time.Hour
	m := d % time.Hour / time.Minute
	ms := d % time.Minute / time.Millisecond
	sec := fmt.Sprintf("%02d", ms/1000)
	if frac := ms % 1000; frac != 0 {
		sec += strings.TrimRight(fmt.Sprintf(".%03d", frac), "0")
	}
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%ss", h, m, sec)
	case m > 0:
		return fmt.Sprintf("%dm%ss", m, sec)
	}
	return strings.TrimPrefix(sec, "0") + "s"
}

// safeName keeps letters, digits, dots, dashes and underscores, and
// replaces anything else with an underscore.
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return '_'
	}, s)
	if s == "" || strings.Trim(s, ".") == "" {
		return "clip"
	}
	return s
}

// transcriptPath is the transcript's path next to the clip: the clip's
// name with its extension replaced by ".json".
func transcriptPath(clip string) string {
	return strings.TrimSuffix(clip, filepath.Ext(clip)) + ".json"
}

// formatSummary is what clipper render prints after a render: the render's
// own summary, then where the clip and the transcript went.
func formatSummary(res render.Result, out, transcript string) string {
	return res.Summary() +
		fmt.Sprintf("%-14s%s (%s)\n", "output", out, render.FormatBytes(res.Size)) +
		fmt.Sprintf("%-14s%s\n", "transcript", transcript)
}

// moveFile moves src to dst, replacing dst. It copies when a rename is not
// possible, for example from the temporary directory to another file
// system.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, in)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("move %s to %s: %w", src, dst, err)
	}
	return os.Remove(src)
}
