package render

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/clipper/download"
	"github.com/salehjafarli/pogfarm/internal/clipper/layout"
)

func TestRequestValidate(t *testing.T) {
	const s = time.Second
	tests := []struct {
		name string
		req  Request
		want string // empty: valid
	}{
		{"vod range", Request{Platform: "twitch", VODID: "1", Start: 10 * s, End: 55 * s}, ""},
		{"input whole", Request{Input: "a.mp4"}, ""},
		{"input range", Request{Input: "a.mp4", Start: s, End: 4 * s}, ""},
		{"input from zero", Request{Input: "a.mp4", End: 4 * s}, ""},
		{"neither", Request{Start: s, End: 2 * s}, "name a source"},
		{"both", Request{Platform: "twitch", VODID: "1", Input: "a.mp4", Start: s, End: 2 * s}, "not both"},
		{"vod without platform", Request{VODID: "1", Start: s, End: 2 * s}, "has no platform"},
		{"vod without range", Request{Platform: "twitch", VODID: "1"}, "a VOD needs a range"},
		{"end before start", Request{Input: "a.mp4", Start: 5 * s, End: 4 * s}, "end 4s is not after start 5s"},
		{"end equals start", Request{Input: "a.mp4", Start: 4 * s, End: 4 * s}, "is not after start"},
		{"start without end", Request{Input: "a.mp4", Start: 4 * s}, "start 4s given without an end"},
		{"negative start", Request{Input: "a.mp4", Start: -s, End: s}, "start -1s is negative"},
		{"negative end", Request{Input: "a.mp4", End: -s}, "end -1s is negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.want == "" {
				if err != nil {
					t.Errorf("valid request refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error containing %q", err, tt.want)
			}
			if !errors.Is(err, download.ErrInvalidRequest) || !IsPermanent(err) {
				t.Errorf("an invalid request should be permanent: %v", err)
			}
		})
	}
}

func TestRequestLength(t *testing.T) {
	if got := (Request{Start: time.Second, End: 4 * time.Second}).Length(); got != 3*time.Second {
		t.Errorf("Length = %s", got)
	}
	if got := (Request{Input: "a.mp4"}).Length(); got != 0 {
		t.Errorf("whole file Length = %s", got)
	}
}

func TestPermanentErrors(t *testing.T) {
	for _, err := range []error{download.ErrVODUnavailable, download.ErrSubscriberOnly, download.ErrUnknownPlatform, ErrTooLong} {
		if !IsPermanent(err) || !errors.Is(err, ErrPermanent) {
			t.Errorf("%v should be permanent", err)
		}
	}
	if IsPermanent(ErrVerification) || IsPermanent(errors.New("network")) {
		t.Error("other errors should be retryable")
	}
}

func TestSummary(t *testing.T) {
	res := Result{
		Duration: 3021 * time.Millisecond, Width: 1080, Height: 1920, Size: 1_234_567,
		Detector: "off", Detection: layout.NoWebcam(),
		Layout:       layout.Result{Arrangement: layout.BlurFill},
		CaptionLines: 4,
		Steps: []StepTime{
			{StepDownload, 1234 * time.Millisecond}, {StepDetect, 0}, {StepLayout, 31 * time.Millisecond},
			{StepTranscribe, 12345 * time.Millisecond}, {StepCaption, time.Millisecond},
			{StepEncode, 8 * time.Second}, {StepVerify, 40 * time.Millisecond},
		},
		Tools: map[string]string{"ffmpeg": "6.1.1"},
	}
	want := "" +
		"arrangement   blur_fill\n" +
		"webcam        none (detector off)\n" +
		"caption lines 4\n" +
		"clip          3s, 1080x1920, 1.2 MB\n" +
		"steps         download 1.2s, detect 0s, layout 30ms, transcribe 12.3s, caption 0s, encode 8s, verify 40ms (total 21.7s)\n" +
		"ffmpeg        6.1.1\n"
	if got := res.Summary(); got != want {
		t.Errorf("summary:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSummaryWebcam(t *testing.T) {
	region := layout.Region{X: 0.75, Y: 0.05, W: 0.2, H: 0.25}
	tests := []struct {
		name string
		res  Result
		want string
	}{
		{"stack", Result{Detector: "yunet", Detection: layout.WebcamAt(region),
			Layout: layout.Result{Arrangement: layout.Stack, WebcamCrop: layout.Crop{X: 1440, Y: 76, W: 384, H: 226}}},
			"x=0.75 y=0.05 w=0.20 h=0.25, crop 384x226 at 1440,76 (detector yunet)"},
		{"fallback", Result{Detector: "yunet", Detection: layout.WebcamAt(region),
			Layout: layout.Result{Arrangement: layout.BlurFill, Fallback: "too small"}},
			"found at x=0.75 y=0.05 w=0.20 h=0.25 but not used: too small (detector yunet)"},
		{"error", Result{Detector: "yunet", DetectionError: "timed out", Layout: layout.Result{Arrangement: layout.BlurFill}},
			"none (detector yunet failed: timed out)"},
	}
	for _, tt := range tests {
		if got := tt.res.webcamSummary(); got != tt.want {
			t.Errorf("%s:\n got %s\nwant %s", tt.name, got, tt.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{{0, "0 B"}, {999, "999 B"}, {4700, "4.7 kB"}, {12_345_678, "12.3 MB"}, {45_000_000, "45.0 MB"}, {2_500_000_000, "2.5 GB"}}
	for _, tt := range tests {
		if got := FormatBytes(tt.n); got != tt.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestProgressLogsEveryTenth(t *testing.T) {
	var buf bytes.Buffer
	p := progress{length: 10 * time.Second, log: slog.New(slog.NewTextHandler(&buf, nil))}
	for _, line := range []string{
		"frame=1", "out_time_us=N/A", "out_time_us=500000", "out_time_us=1000000", "out_time_us=1200000",
		"out_time_us=3500000", "progress=continue", "out_time_us=10000000", "out_time_us=10400000",
	} {
		p.line(line)
	}
	got := strings.Count(buf.String(), "render: encoding")
	if got != 3 || !strings.Contains(buf.String(), "percent=10") || !strings.Contains(buf.String(), "percent=30") ||
		!strings.Contains(buf.String(), "percent=100") {
		t.Errorf("want logs at 10, 30 and 100 percent, got %d:\n%s", got, buf.String())
	}
}

func TestMarshalTranscript(t *testing.T) {
	data, err := MarshalTranscript(fixedTranscript())
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "language": "en",
  "words": [
    {
      "text": "hello",
      "start": 0.2,
      "end": 0.6
    },
    {
      "text": "chat",
      "start": 0.7,
      "end": 1.1
    },
    {
      "text": "look",
      "start": 1.5,
      "end": 1.8
    },
    {
      "text": "at",
      "start": 1.8,
      "end": 1.9
    },
    {
      "text": "this",
      "start": 1.9,
      "end": 2.4
    }
  ]
}
`
	if string(data) != want {
		t.Errorf("got:\n%s\nwant:\n%s", data, want)
	}
}
