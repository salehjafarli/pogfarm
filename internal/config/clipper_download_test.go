package config

import (
	"strings"
	"testing"
	"time"
)

func clipperEnv(t *testing.T) {
	t.Helper()
	cleanEnv(t)
	t.Setenv("CLIPPER_API_SECRET", goodSecret)
	t.Setenv("CLIPPER_POGFARM_URL", "http://pogfarm:8080")
	t.Setenv("CLIPPER_NAME", "node-a")
}

func TestClipperDownloadDefaults(t *testing.T) {
	clipperEnv(t)
	cfg, err := LoadClipper("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Download != defaultClipperDownload() {
		t.Errorf("download defaults:\n got %+v\nwant %+v", cfg.Download, defaultClipperDownload())
	}
	if cfg.Download.Quality != "1080p60" || cfg.Download.FFmpegPath != "ffmpeg" {
		t.Errorf("unexpected defaults: %+v", cfg.Download)
	}
}

func TestClipperDownloadFromEnv(t *testing.T) {
	clipperEnv(t)
	t.Setenv("CLIPPER_DOWNLOAD_TWITCH_DOWNLOADER_PATH", "/opt/td/TwitchDownloaderCLI")
	t.Setenv("CLIPPER_DOWNLOAD_FFMPEG_PATH", "/usr/bin/ffmpeg")
	t.Setenv("CLIPPER_DOWNLOAD_FFPROBE_PATH", "/usr/bin/ffprobe")
	t.Setenv("CLIPPER_DOWNLOAD_QUALITY", "720p60")
	t.Setenv("CLIPPER_DOWNLOAD_TIMEOUT_BASE", "1m")
	t.Setenv("CLIPPER_DOWNLOAD_TIMEOUT_PER_SECOND", "3s")

	cfg, err := LoadClipper("")
	if err != nil {
		t.Fatal(err)
	}
	want := ClipperDownload{
		TwitchDownloaderPath: "/opt/td/TwitchDownloaderCLI",
		FFmpegPath:           "/usr/bin/ffmpeg",
		FFprobePath:          "/usr/bin/ffprobe",
		Quality:              "720p60",
		TimeoutBase:          time.Minute,
		TimeoutPerSecond:     3 * time.Second,
	}
	if cfg.Download != want {
		t.Errorf("got %+v, want %+v", cfg.Download, want)
	}
}

func TestClipperDownloadFromFile(t *testing.T) {
	clipperEnv(t)
	path := writeFile(t, "config.yaml", `
clipper:
  download:
    quality: 480p30
    timeout_per_second: 10s
`)
	cfg, err := LoadClipper(path)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultClipperDownload()
	want.Quality = "480p30"
	want.TimeoutPerSecond = 10 * time.Second
	if cfg.Download != want {
		t.Errorf("got %+v, want %+v", cfg.Download, want)
	}
}

func TestClipperDownloadValidation(t *testing.T) {
	clipperEnv(t)
	path := writeFile(t, "config.yaml", `
clipper:
  download:
    twitch_downloader_path: ""
    ffmpeg_path: " "
    ffprobe_path: ""
    quality: ""
    timeout_base: 0s
    timeout_per_second: -1s
`)
	_, err := LoadClipper(path)
	got := problemsOf(t, err)
	want := []string{
		"clipper.download.twitch_downloader_path (CLIPPER_DOWNLOAD_TWITCH_DOWNLOADER_PATH) must not be empty",
		"clipper.download.ffmpeg_path (CLIPPER_DOWNLOAD_FFMPEG_PATH) must not be empty",
		"clipper.download.ffprobe_path (CLIPPER_DOWNLOAD_FFPROBE_PATH) must not be empty",
		"clipper.download.quality (CLIPPER_DOWNLOAD_QUALITY) must not be empty",
		"clipper.download.timeout_base (CLIPPER_DOWNLOAD_TIMEOUT_BASE) must be a positive duration",
		"clipper.download.timeout_per_second (CLIPPER_DOWNLOAD_TIMEOUT_PER_SECOND) must be a positive duration",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d problems, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("problem %d:\n got %s\nwant prefix %s", i, got[i], want[i])
		}
	}
}
