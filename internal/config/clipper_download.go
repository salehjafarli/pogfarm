package config

import (
	"fmt"
	"strings"
	"time"
)

// ClipperDownload is the "download" section of the clipper's configuration:
// how the source range of a render is fetched (package
// internal/clipper/download).
type ClipperDownload struct {
	// TwitchDownloaderPath is the TwitchDownloaderCLI program that
	// downloads a range of a Twitch VOD: a path, or a name looked up in
	// PATH.
	TwitchDownloaderPath string `yaml:"twitch_downloader_path" env:"CLIPPER_DOWNLOAD_TWITCH_DOWNLOADER_PATH"`
	// FFmpegPath is the ffmpeg program that trims the downloaded range to
	// the exact start and end.
	FFmpegPath string `yaml:"ffmpeg_path" env:"CLIPPER_DOWNLOAD_FFMPEG_PATH"`
	// FFprobePath is the ffprobe program that checks the result.
	FFprobePath string `yaml:"ffprobe_path" env:"CLIPPER_DOWNLOAD_FFPROBE_PATH"`
	// Quality is the Twitch stream quality asked for, as TwitchDownloaderCLI
	// names it, for example "1080p60" or "720p60". When the VOD has no
	// stream of that name, TwitchDownloaderCLI takes the best one.
	Quality string `yaml:"quality" env:"CLIPPER_DOWNLOAD_QUALITY"`
	// TimeoutBase and TimeoutPerSecond bound one download: a tool is killed
	// after TimeoutBase plus TimeoutPerSecond for every second of the
	// requested range.
	TimeoutBase      time.Duration `yaml:"timeout_base" env:"CLIPPER_DOWNLOAD_TIMEOUT_BASE"`
	TimeoutPerSecond time.Duration `yaml:"timeout_per_second" env:"CLIPPER_DOWNLOAD_TIMEOUT_PER_SECOND"`
}

func defaultClipperDownload() ClipperDownload {
	return ClipperDownload{
		TwitchDownloaderPath: "TwitchDownloaderCLI",
		FFmpegPath:           "ffmpeg",
		FFprobePath:          "ffprobe",
		Quality:              "1080p60",
		TimeoutBase:          2 * time.Minute,
		TimeoutPerSecond:     5 * time.Second,
	}
}

func (d ClipperDownload) validate() []string {
	var problems []string
	notEmpty := func(key, env, v string) {
		if strings.TrimSpace(v) == "" {
			problems = append(problems, fmt.Sprintf("clipper.download.%s (%s) must not be empty", key, env))
		}
	}
	notEmpty("twitch_downloader_path", "CLIPPER_DOWNLOAD_TWITCH_DOWNLOADER_PATH", d.TwitchDownloaderPath)
	notEmpty("ffmpeg_path", "CLIPPER_DOWNLOAD_FFMPEG_PATH", d.FFmpegPath)
	notEmpty("ffprobe_path", "CLIPPER_DOWNLOAD_FFPROBE_PATH", d.FFprobePath)
	notEmpty("quality", "CLIPPER_DOWNLOAD_QUALITY", d.Quality)
	if d.TimeoutBase <= 0 {
		problems = append(problems, fmt.Sprintf(
			"clipper.download.timeout_base (CLIPPER_DOWNLOAD_TIMEOUT_BASE) must be a positive duration such as \"2m\": got %s", d.TimeoutBase))
	}
	if d.TimeoutPerSecond <= 0 {
		problems = append(problems, fmt.Sprintf(
			"clipper.download.timeout_per_second (CLIPPER_DOWNLOAD_TIMEOUT_PER_SECOND) must be a positive duration such as \"5s\": got %s", d.TimeoutPerSecond))
	}
	return problems
}
