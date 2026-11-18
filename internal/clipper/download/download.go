package download

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
)

// PlatformTwitch is the platform name of Twitch VODs.
const PlatformTwitch = "twitch"

// DurationTolerance is how far the duration ffprobe reports may be from the
// requested length. It covers one frame at 10 fps or more plus the padding of
// the last AAC frame (1024 samples), which can make a container a few tens of
// milliseconds longer than its video.
const DurationTolerance = 200 * time.Millisecond

// Request names the source and the range of one download.
type Request struct {
	// Platform and VODID name a VOD on a stream platform, for example
	// "twitch" and "2412345678". They are ignored when Input is set.
	Platform string
	VODID    string
	// Input is a local video file to use instead of a platform VOD.
	Input string
	// Start and End are the range, measured from the start of the source.
	// For a VOD both are required and End > Start. For a local file both
	// zero means the whole file; otherwise End > Start.
	Start time.Duration
	End   time.Duration
}

// HasRange reports whether a range was given: End is not zero.
func (r Request) HasRange() bool { return r.End != 0 }

// Length is End - Start, or zero when there is no range.
func (r Request) Length() time.Duration {
	if !r.HasRange() {
		return 0
	}
	return r.End - r.Start
}

// Result is a downloaded range.
type Result struct {
	// Path is the video file, inside the work directory.
	Path string
	// Duration is the file's duration as ffprobe reports it.
	Duration time.Duration
}

// Downloader gets one range of a source into workDir. workDir must exist;
// the downloader writes its files there and never deletes it.
type Downloader interface {
	Download(ctx context.Context, req Request, workDir string) (Result, error)
}

// Service picks the Downloader for a request. It is safe for concurrent use.
type Service struct {
	local     Downloader
	platforms map[string]Downloader
}

// New returns the downloaders configured by cfg: the local-file one and one
// per supported platform.
func New(cfg config.ClipperDownload) *Service {
	t := tools{
		ffmpeg:  cfg.FFmpegPath,
		ffprobe: cfg.FFprobePath,
		timeout: timeoutFunc(cfg.TimeoutBase, cfg.TimeoutPerSecond),
	}
	return &Service{
		local: &Local{tools: t},
		platforms: map[string]Downloader{
			PlatformTwitch: &Twitch{
				Program: cfg.TwitchDownloaderPath,
				Quality: cfg.Quality,
				tools:   t,
			},
		},
	}
}

// Download gets the range req asks for into workDir. A request with Input
// goes to the local-file downloader, any other to the downloader of its
// platform; an unknown platform is ErrUnknownPlatform.
func (s *Service) Download(ctx context.Context, req Request, workDir string) (Result, error) {
	d, err := s.For(req)
	if err != nil {
		return Result{}, err
	}
	return d.Download(ctx, req, workDir)
}

// For returns the Downloader that serves req.
func (s *Service) For(req Request) (Downloader, error) {
	if req.Input != "" {
		return s.local, nil
	}
	d, ok := s.platforms[req.Platform]
	if !ok {
		return nil, fmt.Errorf("download: platform %q: %w", req.Platform, ErrUnknownPlatform)
	}
	return d, nil
}

// ErrPermanent matches every error that a retry will not fix. Use
// errors.Is(err, ErrPermanent), or IsPermanent.
var ErrPermanent = errors.New("permanent failure")

// The known permanent failures. Each also matches ErrPermanent.
var (
	// ErrUnknownPlatform: no downloader exists for the request's platform.
	ErrUnknownPlatform = permanent("unknown platform")
	// ErrInvalidRequest: the request itself is wrong, for example a range
	// whose start is not before its end, or a malformed VOD id.
	ErrInvalidRequest = permanent("invalid request")
	// ErrInputMissing: the local input file does not exist or is not a
	// regular file.
	ErrInputMissing = permanent("input file missing")
	// ErrVODUnavailable: the VOD is deleted or expired.
	ErrVODUnavailable = permanent("VOD deleted or expired")
	// ErrSubscriberOnly: the VOD needs a subscription to watch.
	ErrSubscriberOnly = permanent("VOD is subscriber-only")
)

// IsPermanent reports whether err is a failure that a retry will not fix.
// Any other error is worth retrying, possibly on another node.
func IsPermanent(err error) bool { return errors.Is(err, ErrPermanent) }

type permanentError struct{ msg string }

func permanent(msg string) error { return &permanentError{msg} }

func (e *permanentError) Error() string        { return e.msg }
func (e *permanentError) Is(target error) bool { return target == ErrPermanent }

// timeoutFunc returns the tool timeout for a range of the given length.
func timeoutFunc(base, perSecond time.Duration) func(time.Duration) time.Duration {
	return func(length time.Duration) time.Duration {
		if length < 0 {
			length = 0
		}
		return base + time.Duration(length.Seconds()*float64(perSecond))
	}
}

// validateRange checks the range of a request that must have one.
func validateRange(req Request) error {
	switch {
	case req.Start < 0:
		return fmt.Errorf("download: start %s is negative: %w", req.Start, ErrInvalidRequest)
	case req.End <= req.Start:
		return fmt.Errorf("download: start %s is not before end %s: %w", req.Start, req.End, ErrInvalidRequest)
	}
	return nil
}
