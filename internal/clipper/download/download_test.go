package download

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
)

func testConfig() config.ClipperDownload {
	return config.ClipperDownload{
		TwitchDownloaderPath: "TwitchDownloaderCLI",
		FFmpegPath:           "ffmpeg",
		FFprobePath:          "ffprobe",
		Quality:              "1080p60",
		TimeoutBase:          2 * time.Minute,
		TimeoutPerSecond:     5 * time.Second,
	}
}

func TestTwitchArgs(t *testing.T) {
	tests := []struct {
		name   string
		ffmpeg string
		start  time.Duration
		end    time.Duration
		want   string
	}{
		{
			name:   "ffmpeg from PATH",
			ffmpeg: "ffmpeg",
			start:  time.Hour + 2*time.Minute + 10*time.Second,
			end:    time.Hour + 2*time.Minute + 55*time.Second + 500*time.Millisecond,
			want: "videodownload --id 2412345678 --output /w/twitch-raw.mp4 --quality 1080p60 " +
				"--beginning 3730000ms --ending 3775500ms --trim-mode Exact --temp-path /w/twitch-tmp --collision Overwrite",
		},
		{
			name:   "ffmpeg as a path",
			ffmpeg: "/usr/bin/ffmpeg",
			start:  0,
			end:    20 * time.Second,
			want: "videodownload --id 2412345678 --output /w/twitch-raw.mp4 --quality 1080p60 " +
				"--beginning 0ms --ending 20000ms --trim-mode Exact --temp-path /w/twitch-tmp --collision Overwrite " +
				"--ffmpeg-path /usr/bin/ffmpeg",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(twitchArgs("2412345678", "/w/twitch-raw.mp4", "/w/twitch-tmp", "1080p60", tt.ffmpeg, tt.start, tt.end), " ")
			if got != tt.want {
				t.Errorf("\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestTrimArgs(t *testing.T) {
	got := strings.Join(trimArgs("/w/in.mkv", "/w/source.mp4", 3500*time.Millisecond, 7700*time.Millisecond), " ")
	want := "-hide_banner -nostdin -loglevel error -y -ss 3.5 -i /w/in.mkv -t 7.7 " +
		"-map 0:v:0 -map 0:a:0 -c:v libx264 -preset veryfast -crf 18 -pix_fmt yuv420p " +
		"-c:a aac -b:a 192k -avoid_negative_ts make_zero -movflags +faststart /w/source.mp4"
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

func TestProbeArgs(t *testing.T) {
	got := strings.Join(probeArgs("/w/source.mp4"), " ")
	want := "-v error -show_entries format=duration:stream=codec_type,duration -of json /w/source.mp4"
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

func TestSeconds(t *testing.T) {
	tests := map[time.Duration]string{
		0:                         "0",
		3500 * time.Millisecond:   "3.5",
		11200 * time.Millisecond:  "11.2",
		time.Hour + time.Second:   "3601",
		1234567 * time.Nanosecond: "0.001235",
	}
	for d, want := range tests {
		if got := seconds(d); got != want {
			t.Errorf("seconds(%s) = %s, want %s", d, got, want)
		}
	}
}

func TestTimeoutIsProportional(t *testing.T) {
	f := timeoutFunc(2*time.Minute, 5*time.Second)
	tests := map[time.Duration]time.Duration{
		0:                       0 + 2*time.Minute,
		-time.Second:            2 * time.Minute,
		45 * time.Second:        2*time.Minute + 225*time.Second,
		90 * time.Second:        2*time.Minute + 450*time.Second,
		1500 * time.Millisecond: 2*time.Minute + 7500*time.Millisecond,
	}
	for length, want := range tests {
		if got := f(length); got != want {
			t.Errorf("timeout(%s) = %s, want %s", length, got, want)
		}
	}
}

func TestParseProbe(t *testing.T) {
	p, err := parseProbe([]byte(`{"programs":[],"streams":[{"codec_type":"video","duration":"7.680000"},{"codec_type":"audio","duration":"7.701333"},{"codec_type":"data"}],"format":{"duration":"7.701333"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasVideo || !p.HasAudio || p.Duration != 7701333*time.Microsecond || p.VideoDuration != 7680*time.Millisecond {
		t.Errorf("got %+v", p)
	}
	p, err = parseProbe([]byte(`{"streams":[{"codec_type":"video","duration":"N/A"}],"format":{"duration":"3.5"}}`))
	if err != nil || !p.HasVideo || p.VideoDuration != 0 || p.Duration != 3500*time.Millisecond {
		t.Errorf("video duration not recorded: got %+v, %v", p, err)
	}
	p, err = parseProbe([]byte(`{"streams":[{"codec_type":"audio"}],"format":{}}`))
	if err != nil || p.HasVideo || !p.HasAudio || p.Duration != 0 {
		t.Errorf("audio only: got %+v, %v", p, err)
	}
	for _, bad := range []string{`not json`, `{"format":{"duration":"N/A"}}`, `{"format":{"duration":"-1"}}`} {
		if _, err := parseProbe([]byte(bad)); err == nil {
			t.Errorf("parseProbe(%s): want an error", bad)
		}
	}
}

func pr(d time.Duration, video, audio bool) probe {
	return probe{Duration: d, HasVideo: video, HasAudio: audio}
}

func TestCheckProbe(t *testing.T) {
	ok := probe{Duration: 7700 * time.Millisecond, HasVideo: true, HasAudio: true}
	tests := []struct {
		name string
		p    probe
		want time.Duration
		ok   bool
	}{
		{"exact", ok, 7700 * time.Millisecond, true},
		{"within tolerance above", pr(7700*time.Millisecond+DurationTolerance, true, true), 7700 * time.Millisecond, true},
		{"within tolerance below", pr(7700*time.Millisecond-DurationTolerance, true, true), 7700 * time.Millisecond, true},
		{"too long", pr(7700*time.Millisecond+DurationTolerance+time.Millisecond, true, true), 7700 * time.Millisecond, false},
		{"too short", pr(7*time.Second, true, true), 7700 * time.Millisecond, false},
		{"no video", pr(7700*time.Millisecond, false, true), 7700 * time.Millisecond, false},
		{"no audio", pr(7700*time.Millisecond, true, false), 7700 * time.Millisecond, false},
		{"whole file", pr(20*time.Second, true, true), 0, true},
		{"whole file empty", pr(0, true, true), 0, false},
	}
	for _, tt := range tests {
		err := checkProbe(tt.p, tt.want)
		if (err == nil) != tt.ok {
			t.Errorf("%s: got %v, want ok=%v", tt.name, err, tt.ok)
		}
		if err != nil && IsPermanent(err) {
			t.Errorf("%s: a failed verification must be retryable: %v", tt.name, err)
		}
	}
}

func TestTwitchLeadIn(t *testing.T) {
	const length = 45 * time.Second
	tests := []struct {
		got     time.Duration
		want    time.Duration
		wantErr bool
	}{
		{length, 0, false},
		{length + 1840*time.Millisecond, 1840 * time.Millisecond, false},
		{length - 100*time.Millisecond, 0, false},
		{length + maxLeadIn, maxLeadIn, false},
		{length + maxLeadIn + time.Millisecond, 0, true},
		{length - time.Second, 0, true},
	}
	for _, tt := range tests {
		got, err := twitchLeadIn(tt.got, length)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("twitchLeadIn(%s) = %s, %v; want %s, error %v", tt.got, got, err, tt.want, tt.wantErr)
		}
		if err != nil && IsPermanent(err) {
			t.Errorf("twitchLeadIn(%s): must be retryable: %v", tt.got, err)
		}
	}
}

func TestClassifyTwitch(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		stderr string
		want   error
	}{
		{"deleted", "", "Unhandled exception. System.AggregateException: One or more errors occurred. (Invalid VOD, deleted/expired VOD possibly?)", ErrVODUnavailable},
		{"subscriber only", "[ERROR] - Insufficient access to VOD, OAuth may be required.", "", ErrSubscriberOnly},
		{"bad id", "[ERROR] - Unable to parse Vod ID/URL.", "", ErrInvalidRequest},
		{"network", "", "System.Net.Http.HttpRequestException: Connection refused", nil},
		{"no ffmpeg", "[ERROR] - Unable to find FFmpeg, exiting.", "", nil},
		{"empty", "", "", nil},
	}
	for _, tt := range tests {
		got := classifyTwitch([]byte(tt.stdout), []byte(tt.stderr))
		if got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestPermanentErrors(t *testing.T) {
	for _, err := range []error{ErrUnknownPlatform, ErrInvalidRequest, ErrInputMissing, ErrVODUnavailable, ErrSubscriberOnly} {
		wrapped := errors.Join(errors.New("context"), err)
		if !IsPermanent(err) || !IsPermanent(wrapped) || !errors.Is(wrapped, ErrPermanent) {
			t.Errorf("%v must be permanent", err)
		}
	}
	for _, err := range []error{errVerify, context.Canceled, context.DeadlineExceeded, errors.New("boom")} {
		if IsPermanent(err) {
			t.Errorf("%v must be retryable", err)
		}
	}
}

func TestUnknownPlatform(t *testing.T) {
	s := New(testConfig())
	_, err := s.Download(context.Background(), Request{Platform: "kick", VODID: "1", Start: 0, End: time.Second}, t.TempDir())
	if !errors.Is(err, ErrUnknownPlatform) || !IsPermanent(err) {
		t.Errorf("got %v, want a permanent ErrUnknownPlatform", err)
	}
	_, err = s.Download(context.Background(), Request{VODID: "1", End: time.Second}, t.TempDir())
	if !errors.Is(err, ErrUnknownPlatform) {
		t.Errorf("empty platform: got %v, want ErrUnknownPlatform", err)
	}
}

func TestServiceRouting(t *testing.T) {
	s := New(testConfig())
	d, err := s.For(Request{Platform: PlatformTwitch, VODID: "1", Input: "/some/file.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(*Local); !ok {
		t.Errorf("a request with Input must go to the local downloader, got %T", d)
	}
	d, err = s.For(Request{Platform: PlatformTwitch, VODID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	tw, ok := d.(*Twitch)
	if !ok || tw.Program != "TwitchDownloaderCLI" || tw.Quality != "1080p60" {
		t.Errorf("twitch downloader not built from config: %#v", d)
	}
}

func TestMissingInput(t *testing.T) {
	s := New(testConfig())
	dir := t.TempDir()
	for name, in := range map[string]string{
		"missing":   filepath.Join(dir, "nope.mp4"),
		"directory": dir,
	} {
		for _, r := range []Request{{Input: in}, {Input: in, Start: time.Second, End: 2 * time.Second}} {
			_, err := s.Download(context.Background(), r, t.TempDir())
			if !errors.Is(err, ErrInputMissing) || !IsPermanent(err) {
				t.Errorf("%s %+v: got %v, want a permanent ErrInputMissing", name, r, err)
			}
		}
	}
}

func TestInvalidRange(t *testing.T) {
	s := New(testConfig())
	in := filepath.Join(t.TempDir(), "in.mp4")
	if err := os.WriteFile(in, []byte("not checked before the range"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		req  Request
	}{
		{"local start equals end", Request{Input: in, Start: 5 * time.Second, End: 5 * time.Second}},
		{"local start after end", Request{Input: in, Start: 6 * time.Second, End: 5 * time.Second}},
		{"local start without end", Request{Input: in, Start: 5 * time.Second}},
		{"local negative start", Request{Input: in, Start: -time.Second, End: 5 * time.Second}},
		{"twitch start equals end", Request{Platform: PlatformTwitch, VODID: "1", Start: 5 * time.Second, End: 5 * time.Second}},
		{"twitch start after end", Request{Platform: PlatformTwitch, VODID: "1", Start: 6 * time.Second, End: 5 * time.Second}},
		{"twitch no range", Request{Platform: PlatformTwitch, VODID: "1"}},
		{"twitch end rounds onto start", Request{Platform: PlatformTwitch, VODID: "1", Start: time.Second, End: time.Second + 100*time.Microsecond}},
		{"twitch bad id", Request{Platform: PlatformTwitch, VODID: "v123", End: time.Second}},
		{"twitch empty id", Request{Platform: PlatformTwitch, End: time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.Download(context.Background(), tt.req, t.TempDir())
			if !errors.Is(err, ErrInvalidRequest) || !IsPermanent(err) {
				t.Errorf("got %v, want a permanent ErrInvalidRequest", err)
			}
		})
	}
}
