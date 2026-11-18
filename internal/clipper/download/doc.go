// Package download gets exactly the part of a stream a render asks for, as a
// video file in the render's work directory. It is the first step of the
// render core (internal/clipper/render).
//
// # Use
//
//	d := download.New(cfg.Download)
//	res, err := d.Download(ctx, download.Request{
//		Platform: download.PlatformTwitch, VODID: "2412345678",
//		Start: 62*time.Minute + 10*time.Second, End: 62*time.Minute + 55*time.Second,
//	}, workDir)
//
// A request names either a platform VOD (Platform and VODID) or a local file
// (Input). Service.Download picks the Downloader for it: the local-file
// downloader when Input is set, otherwise the one registered for the
// platform. Only Twitch exists today; another platform is a sibling
// implementation of Downloader (for example one wrapping yt-dlp) added to the
// registry in New.
//
// # Exact range
//
// Every result holds exactly [Start, End) of the source. A stream copy can
// only cut on keyframes, so both downloaders end with one ffmpeg pass that
// seeks with -ss before -i and re-encodes (libx264, AAC). With re-encoding,
// ffmpeg decodes from the preceding keyframe and drops the frames before the
// requested start, so the cut is frame-accurate at both ends.
//
// TwitchDownloaderCLI (videodownload, --trim-mode Exact) cuts the start at a
// keyframe at or before the requested beginning, but places the end exactly:
// its final ffmpeg call uses -ss and -t as input options with stream copy, and
// -t counts from the requested beginning, not from the keyframe. Its output is
// therefore the requested range plus an unknown lead-in. The Twitch downloader
// measures the output with ffprobe and cuts lead-in = duration - length off
// the front. If TwitchDownloaderCLI happened to cut exactly, the lead-in is
// zero and the result is the same.
//
// # Verification
//
// The result is checked with ffprobe: the file exists, has a video and an
// audio stream, and when a range was asked for its duration is within
// DurationTolerance of End - Start. A local file copied whole only needs a
// positive duration.
//
// # Errors
//
// Failures that will not go away on a retry are permanent: errors.Is(err,
// ErrPermanent) reports them, and IsPermanent is the same test. The specific
// causes are ErrUnknownPlatform, ErrInvalidRequest, ErrInputMissing,
// ErrVODUnavailable (deleted or expired) and ErrSubscriberOnly; each also
// matches ErrPermanent. Everything else (network trouble, a tool that
// crashed or timed out, a result that failed verification) is retryable. A
// cancelled ctx gives an error that matches context.Canceled.
//
// # Tools
//
// ffmpeg, ffprobe and TwitchDownloaderCLI run through internal/runner, each
// with a timeout of TimeoutBase plus TimeoutPerSecond per second of the range.
// The tested TwitchDownloaderCLI version is 1.56.5, the one in the clipper
// base image. Its flags were taken from its 1.56.5 source
// (TwitchDownloaderCLI/Modes/Arguments/VideoDownloadArgs.cs and the CLI
// README) and its error messages from TwitchDownloaderCore/VideoDownloader.cs;
// neither has been run against Twitch by the tests.
//
// # Manual test against Twitch
//
// The automated tests use a fake TwitchDownloaderCLI. To run a real download,
// inside the clipper base image or with TwitchDownloaderCLI and ffmpeg in
// PATH, pick a public VOD id and run:
//
//	POGFARM_TEST_TWITCH_VOD=<vod id> go test -run TestTwitchRealDownload -v ./internal/clipper/download
//
// It downloads 1m00s to 1m20s of that VOD into a temporary directory and
// checks the result as above.
package download
