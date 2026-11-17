package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

var leaseTime = time.Date(2026, 11, 17, 14, 32, 0, 0, time.UTC)

func sampleJob() Job {
	return Job{
		ID:                       "42",
		Attempt:                  2,
		Platform:                 PlatformTwitch,
		VODID:                    "2301234567",
		StartSeconds:             1055.5,
		EndSeconds:               1100,
		Streamer:                 Streamer{Login: "somestreamer", DisplayName: "SomeStreamer"},
		LeaseExpiresAt:           leaseTime,
		HeartbeatIntervalSeconds: 30,
	}
}

// roundTrip checks that v marshals to exactly wantJSON and that wantJSON
// unmarshals back to v. The literal JSON pins the wire names: renaming a
// field is a contract change and must fail here.
func roundTrip[T any](t *testing.T, v T, wantJSON string) {
	t.Helper()

	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(wantJSON)); err != nil {
		t.Fatalf("bad wantJSON: %v", err)
	}
	if string(got) != compact.String() {
		t.Errorf("marshal mismatch\n got: %s\nwant: %s", got, compact.String())
	}

	var back T
	dec := json.NewDecoder(strings.NewReader(wantJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back, v) {
		t.Errorf("unmarshal mismatch\n got: %+v\nwant: %+v", back, v)
	}
}

const sampleJobJSON = `{
	"id": "42",
	"attempt": 2,
	"platform": "twitch",
	"vod_id": "2301234567",
	"start_seconds": 1055.5,
	"end_seconds": 1100,
	"streamer": {"login": "somestreamer", "display_name": "SomeStreamer"},
	"lease_expires_at": "2026-11-17T14:32:00Z",
	"heartbeat_interval_seconds": 30
}`

func TestJobRoundTrip(t *testing.T) {
	roundTrip(t, sampleJob(), sampleJobJSON)
}

func TestClaimRoundTrip(t *testing.T) {
	roundTrip(t, ClaimRequest{Clipper: "node-a"}, `{"clipper": "node-a"}`)

	job := sampleJob()
	roundTrip(t, ClaimResponse{Job: &job}, `{"job": `+sampleJobJSON+`}`)
	roundTrip(t, ClaimResponse{RetryAfterSeconds: 10}, `{"job": null, "retry_after_seconds": 10}`)
}

func TestHeartbeatRoundTrip(t *testing.T) {
	roundTrip(t, HeartbeatRequest{Clipper: "node-a"}, `{"clipper": "node-a"}`)
	roundTrip(t, HeartbeatResponse{LeaseExpiresAt: leaseTime}, `{"lease_expires_at": "2026-11-17T14:32:00Z"}`)
}

func TestFailRoundTrip(t *testing.T) {
	roundTrip(t,
		FailRequest{Clipper: "node-a", Reason: "VOD was deleted", Retryable: false},
		`{"clipper": "node-a", "reason": "VOD was deleted", "retryable": false}`)
}

func TestCompleteRoundTrip(t *testing.T) {
	roundTrip(t,
		CompleteMetadata{
			Clipper:         "node-a",
			SHA256:          "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			SizeBytes:       18250000,
			DurationSeconds: 44.5,
			Width:           1080,
			Height:          1920,
			ToolVersions:    map[string]string{"ffmpeg": "7.1"},
		},
		`{
			"clipper": "node-a",
			"sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			"size_bytes": 18250000,
			"duration_seconds": 44.5,
			"width": 1080,
			"height": 1920,
			"tool_versions": {"ffmpeg": "7.1"}
		}`)

	roundTrip(t,
		Transcript{
			Language: "en",
			Words: []Word{
				{Text: "no", StartSeconds: 0.32, EndSeconds: 0.5},
				{Text: "way", StartSeconds: 0.5, EndSeconds: 0.91},
			},
		},
		`{
			"language": "en",
			"words": [
				{"text": "no", "start_seconds": 0.32, "end_seconds": 0.5},
				{"text": "way", "start_seconds": 0.5, "end_seconds": 0.91}
			]
		}`)

	// A clip without speech has an empty word list, not a missing one.
	roundTrip(t, Transcript{Words: []Word{}}, `{"language": "", "words": []}`)

	roundTrip(t, CompleteResponse{Result: ResultAccepted}, `{"result": "accepted"}`)
	roundTrip(t, CompleteResponse{Result: ResultAlreadyCompleted}, `{"result": "already_completed"}`)
}

func TestErrorRoundTrip(t *testing.T) {
	roundTrip(t,
		NewErrorResponse(CodeLeaseLost, "job 42 is held by another clipper"),
		`{"error": {"code": "lease_lost", "message": "job 42 is held by another clipper"}}`)
}

func TestErrorCodeStatus(t *testing.T) {
	cases := map[ErrorCode]int{
		CodeUnauthenticated:    http.StatusUnauthorized,
		CodeInvalidRequest:     http.StatusBadRequest,
		CodeUnknownJob:         http.StatusNotFound,
		CodeLeaseLost:          http.StatusConflict,
		CodeAlreadyCompleted:   http.StatusConflict,
		CodePayloadTooLarge:    http.StatusRequestEntityTooLarge,
		CodeChecksumMismatch:   http.StatusUnprocessableEntity,
		CodeInternal:           http.StatusInternalServerError,
		ErrorCode("something"): http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := code.HTTPStatus(); got != want {
			t.Errorf("%s: status %d, want %d", code, got, want)
		}
	}
}

func TestPaths(t *testing.T) {
	cases := map[string]string{
		PathClaim:           "/v1/jobs/claim",
		PathHeartbeat("42"): "/v1/jobs/42/heartbeat",
		PathComplete("42"):  "/v1/jobs/42/complete",
		PathFail("42"):      "/v1/jobs/42/fail",
		RouteClaim:          "POST /v1/jobs/claim",
		RouteHeartbeat:      "POST /v1/jobs/{id}/heartbeat",
		RouteComplete:       "POST /v1/jobs/{id}/complete",
		RouteFail:           "POST /v1/jobs/{id}/fail",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// The routes must be valid ServeMux patterns, and the path helpers must
// produce paths those patterns match with the job ID in {id}.
func TestRoutesMatchPaths(t *testing.T) {
	mux := http.NewServeMux()
	var gotRoute, gotID string
	for _, route := range []string{RouteClaim, RouteHeartbeat, RouteComplete, RouteFail} {
		mux.HandleFunc(route, func(_ http.ResponseWriter, r *http.Request) {
			gotRoute, gotID = route, r.PathValue("id")
		})
	}

	cases := []struct{ path, route, id string }{
		{PathClaim, RouteClaim, ""},
		{PathHeartbeat("42"), RouteHeartbeat, "42"},
		{PathComplete("42"), RouteComplete, "42"},
		{PathFail("42"), RouteFail, "42"},
	}
	for _, c := range cases {
		gotRoute, gotID = "", ""
		req, err := http.NewRequest(http.MethodPost, c.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		mux.ServeHTTP(httptest.NewRecorder(), req)
		if gotRoute != c.route || gotID != c.id {
			t.Errorf("%s: matched %q with id %q, want %q with id %q", c.path, gotRoute, gotID, c.route, c.id)
		}
	}
}

// Nothing in the job payload may be specific to one platform beyond the
// value of the platform field.
func TestJobIsPlatformNeutral(t *testing.T) {
	data, err := json.Marshal(sampleJob())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["platform"]; !ok {
		t.Error(`job payload has no "platform" field`)
	}
	for name := range fields {
		if strings.Contains(strings.ToLower(name), "twitch") {
			t.Errorf("job payload field %q is Twitch-specific", name)
		}
	}
}

func TestHeartbeatInterval(t *testing.T) {
	if got := sampleJob().HeartbeatInterval(); got != 30*time.Second {
		t.Errorf("got %v, want 30s", got)
	}
}
