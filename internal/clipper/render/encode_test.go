package render

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
	"github.com/salehjafarli/pogfarm/internal/config"
)

// planFor lays out a 1920x1080 source with the default framing settings.
func planFor(t *testing.T, d layout.Detection) layout.Result {
	t.Helper()
	l, err := layout.New(config.ClipperLayout{StackFaceShare: 0.45, CropFaceShare: 0.25, EyeLine: 1.0 / 3})
	if err != nil {
		t.Fatal(err)
	}
	res, err := l.Plan(1920, 1080, d)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The tail of the command line after the filter graph is the same for every
// arrangement.
const encodeTail = " -map [render_v] -map [render_a]" +
	" -c:v libx264 -preset fast -crf 21 -maxrate 2000000 -bufsize 2000000 -profile:v high -pix_fmt yuv420p" +
	" -fps_mode cfr -c:a aac -b:a 128000 -ar 48000 -ac 2 -movflags +faststart /w/clip.mp4"

const encodeHead = "-hide_banner -nostdin -loglevel error -nostats -progress pipe:1 -y -i /w/source.mp4 -filter_complex "

const encodeFilterTail = ";[layout]ass=filename=/w/captions.ass,fps=30,format=yuv420p[render_v]" +
	";[0:a:0]loudnorm=I=-14:TP=-1.5:LRA=11,aresample=48000[render_a]"

// cornerFace is a webcam face in the top-right corner, 8% of the frame
// high; bigFace fills the picture, 30% high.
var (
	cornerFace = layout.Box{X: 0.85, Y: 0.05, W: 0.05, H: 0.08}
	bigFace    = layout.Box{X: 0.4, Y: 0.2, W: 0.15, H: 0.3}
)

func TestEncodeArgs(t *testing.T) {
	tests := []struct {
		name      string
		detection layout.Detection
		arrange   layout.Arrangement
		want      string
	}{
		{
			name:      "blur fill",
			detection: layout.NoFace(layout.ReasonNoFace, ""),
			arrange:   layout.BlurFill,
			want: encodeHead +
				"[0:v]split=2[layout_bg_in][layout_fg_in];" +
				"[layout_bg_in]crop=606:1080:656:0,scale=270:480,boxblur=12:2,scale=1080:1920,setsar=1[layout_bg];" +
				"[layout_fg_in]crop=1920:1080:0:0,scale=1080:608,setsar=1[layout_fg];" +
				"[layout_bg][layout_fg]overlay=0:656,setsar=1[layout]" +
				encodeFilterTail + encodeTail,
		},
		{
			name:      "stack",
			detection: layout.FaceAt(cornerFace, layout.Stack, ""),
			arrange:   layout.Stack,
			want: encodeHead +
				"[0:v]split=2[layout_face_in][layout_game_in];" +
				"[layout_face_in]crop=324:192:1518:24,scale=1080:640,setsar=1[layout_face];" +
				"[layout_game_in]crop=910:1080:504:0,scale=1080:1280,setsar=1[layout_game];" +
				"[layout_face][layout_game]vstack=inputs=2,setsar=1[layout]" +
				encodeFilterTail + encodeTail,
		},
		{
			name:      "face crop",
			detection: layout.FaceAt(bigFace, layout.FaceCrop, ""),
			arrange:   layout.FaceCrop,
			want: encodeHead +
				"[0:v]crop=608:1080:608:0,scale=1080:1920,setsar=1[layout]" +
				encodeFilterTail + encodeTail,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := planFor(t, tt.detection)
			if plan.Arrangement != tt.arrange {
				t.Fatalf("arrangement %s, want %s", plan.Arrangement, tt.arrange)
			}
			args := encodeArgs(encodeSpec{
				Input: "/w/source.mp4", Output: "/w/clip.mp4",
				LayoutFilter: plan.Filter, CaptionPath: "/w/captions.ass",
				Preset: "fast", CRF: 21, FrameRate: 30, MaxRate: 2_000_000,
			})
			if got := strings.Join(args, " "); got != tt.want {
				t.Errorf("command line:\n got %s\nwant %s", got, tt.want)
			}
			// The filter graph is a single argument.
			if i := indexOf(args, "-filter_complex"); i < 0 || !strings.HasPrefix(args[i+1], plan.Filter+";") {
				t.Errorf("-filter_complex is not followed by the layout fragment: %q", args)
			}
		})
	}
}

func TestEncodeArgsEscapesCaptionPath(t *testing.T) {
	args := encodeArgs(encodeSpec{
		Input: "/w/source.mp4", Output: "/w/clip.mp4",
		LayoutFilter: planFor(t, layout.NoFace(layout.ReasonNoFace, "")).Filter, CaptionPath: "/tmp/a b: it's/captions.ass",
		Preset: "fast", CRF: 21, FrameRate: 30, MaxRate: 2_000_000,
	})
	graph := args[indexOf(args, "-filter_complex")+1]
	want := `[layout]ass=filename=/tmp/a b\\: it\\\'s/captions.ass,fps=30`
	if !strings.Contains(graph, want) {
		t.Errorf("filter graph %q does not contain %q", graph, want)
	}
}

func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}

func TestEscapeFilterPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/w/captions.ass", "/w/captions.ass"},
		{"/a b/c.ass", "/a b/c.ass"},
		{"/a:b/c.ass", `/a\\:b/c.ass`},
		{"/it's/c.ass", `/it\\\'s/c.ass`},
		{`/back\slash/c.ass`, `/back\\\\slash/c.ass`},
		{"/[x],y;z/c.ass", `/\[x\]\,y\;z/c.ass`},
		{"/tmp/work dir: it's/captions.ass", `/tmp/work dir\\: it\\\'s/captions.ass`},
		// The example of "Notes on filtergraph escaping" in ffmpeg-filters.
		{
			"this is a 'string': may contain one, or more, special characters",
			`this is a \\\'string\\\'\\: may contain one\, or more\, special characters`,
		},
	}
	for _, tt := range tests {
		if got := EscapeFilterPath(tt.in); got != tt.want {
			t.Errorf("EscapeFilterPath(%q):\n got %s\nwant %s", tt.in, got, tt.want)
		}
	}
}

func TestVideoBitrateCap(t *testing.T) {
	tests := []struct {
		maxBytes int64
		length   time.Duration
		want     int64
	}{
		// (45e6*8*0.95 - 128000*L) / (L+1), rounded down to a kilobit.
		{45_000_000, 3 * time.Second, 85_404_000},
		{45_000_000, 60 * time.Second, 5_480_000},
		{45_000_000, 180 * time.Second, 1_762_000},
		{20_000_000, 45 * time.Second, 3_179_000},
	}
	for _, tt := range tests {
		got, err := VideoBitrateCap(tt.maxBytes, tt.length)
		if err != nil || got != tt.want {
			t.Errorf("VideoBitrateCap(%d, %s) = %d, %v; want %d", tt.maxBytes, tt.length, got, err, tt.want)
		}
	}
}

// TestVideoBitrateCapStaysUnderLimit checks the plan: video at the cap for
// one second more than the clip, plus the audio, fits in 95 % of the limit.
func TestVideoBitrateCapStaysUnderLimit(t *testing.T) {
	for _, maxBytes := range []int64{config.MinRenderMaxBytes * 8, 45_000_000, 100_000_000} {
		for sec := 1; sec <= 600; sec++ {
			length := time.Duration(sec) * time.Second
			v, err := VideoBitrateCap(maxBytes, length)
			if errors.Is(err, ErrTooLong) {
				continue
			}
			if err != nil {
				t.Fatalf("VideoBitrateCap(%d, %s): %v", maxBytes, length, err)
			}
			planned := (float64(v)*float64(sec+1) + AudioBitrate*float64(sec)) / 8
			if planned > float64(maxBytes)*0.95 {
				t.Fatalf("VideoBitrateCap(%d, %s) = %d plans %.0f bytes, over 95%% of the limit", maxBytes, length, v, planned)
			}
		}
	}
}

func TestVideoBitrateCapErrors(t *testing.T) {
	_, err := VideoBitrateCap(config.MinRenderMaxBytes, time.Minute)
	if !errors.Is(err, ErrTooLong) || !IsPermanent(err) {
		t.Errorf("1 MiB for a minute: got %v, want a permanent ErrTooLong", err)
	}
	for _, length := range []time.Duration{0, -time.Second} {
		if _, err := VideoBitrateCap(45_000_000, length); err == nil || errors.Is(err, ErrTooLong) {
			t.Errorf("length %s: got %v, want a plain error", length, err)
		}
	}
}

func TestParseProbe(t *testing.T) {
	out := []byte(`{
	"streams": [
		{"codec_name": "h264", "codec_type": "video", "width": 1080, "height": 1920, "pix_fmt": "yuv420p"},
		{"codec_name": "aac", "codec_type": "audio"},
		{"codec_name": "mov_text", "codec_type": "subtitle"}
	],
	"format": {"duration": "3.0215", "size": "812345"}
}`)
	m, err := parseProbe(out)
	if err != nil {
		t.Fatal(err)
	}
	want := media{
		Duration: 3021500 * time.Microsecond, HasVideo: true, VideoCodec: "h264", Width: 1080, Height: 1920,
		PixFmt: "yuv420p", HasAudio: true, AudioCodec: "aac",
	}
	if m != want {
		t.Errorf("got %+v, want %+v", m, want)
	}
	if _, err := parseProbe([]byte(`{"format": {"duration": "soon"}}`)); err == nil {
		t.Error("a bad duration should be an error")
	}
	if _, err := parseProbe([]byte(`not json`)); err == nil {
		t.Error("bad JSON should be an error")
	}
}

func TestCheckClip(t *testing.T) {
	good := media{
		Duration: 3 * time.Second, HasVideo: true, VideoCodec: "h264", Width: 1080, Height: 1920,
		PixFmt: "yuv420p", HasAudio: true, AudioCodec: "aac",
	}
	const limit = 45_000_000
	if err := checkClip(good, 1_000_000, 3*time.Second, limit); err != nil {
		t.Errorf("good clip: %v", err)
	}
	if err := checkClip(good, limit, 3*time.Second+DurationTolerance, limit); err != nil {
		t.Errorf("clip at the limits: %v", err)
	}
	tests := []struct {
		name string
		mod  func(*media)
		size int64
		want string
	}{
		{"no video", func(m *media) { m.HasVideo = false }, 1, "no video stream"},
		{"vp9", func(m *media) { m.VideoCodec = "vp9" }, 1, `video codec "vp9", want h264`},
		{"landscape", func(m *media) { m.Width, m.Height = 1920, 1080 }, 1, "picture 1920x1080, want 1080x1920"},
		{"yuv444p", func(m *media) { m.PixFmt = "yuv444p" }, 1, `pixel format "yuv444p"`},
		{"no audio", func(m *media) { m.HasAudio = false }, 1, "no audio stream"},
		{"opus", func(m *media) { m.AudioCodec = "opus" }, 1, `audio codec "opus", want aac`},
		{"short", func(m *media) { m.Duration = 2 * time.Second }, 1, "duration 2s, want 3s within 500ms"},
		{"long", func(m *media) { m.Duration = 4 * time.Second }, 1, "duration 4s, want 3s"},
		{"too big", func(*media) {}, limit + 1, "size 45000001 bytes, above the limit of 45000000"},
	}
	for _, tt := range tests {
		m := good
		tt.mod(&m)
		err := checkClip(m, tt.size, 3*time.Second, limit)
		if !errors.Is(err, ErrVerification) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want ErrVerification with %q", tt.name, err, tt.want)
		}
		if IsPermanent(err) {
			t.Errorf("%s: a failed check should be retryable", tt.name)
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ffmpeg version 6.1.1-3ubuntu5 Copyright (c) 2000-2023 the FFmpeg developers\nbuilt with gcc 13\n", "6.1.1-3ubuntu5"},
		{"ffmpeg version n7.1 Copyright\n", "n7.1"},
		{"something else\n", "something else"},
	}
	for _, tt := range tests {
		if got := parseVersion([]byte(tt.in)); got != tt.want {
			t.Errorf("parseVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestEncodeTimeout(t *testing.T) {
	tl := newTools(config.ClipperRender{TimeoutBase: 2 * time.Minute, TimeoutPerSecond: 10 * time.Second})
	if got := tl.encodeTimeout(45 * time.Second); got != 2*time.Minute+450*time.Second {
		t.Errorf("45s clip: timeout %s", got)
	}
	if got := tl.encodeTimeout(-time.Second); got != 2*time.Minute {
		t.Errorf("negative length: timeout %s", got)
	}
}
