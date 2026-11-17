package intake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// part is one part of a hand-built upload.
type part struct {
	name string
	data []byte
}

// multipartBody builds an upload from parts, in the given order.
func multipartBody(t *testing.T, parts []part) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		pw, err := w.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {fmt.Sprintf(`form-data; name=%q`, p.name)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pw.Write(p.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

// post sends body to the complete route of job jobID and returns the status
// and the error code of the answer.
func (f *fixture) post(jobID int64, body []byte, contentType string) (int, api.ErrorCode) {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.server.URL+api.PathComplete(queue.JobID(jobID)), bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var e api.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&e)
	return resp.StatusCode, e.Error.Code
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestHandlerMalformed(t *testing.T) {
	clip := newClip(t, 2000)
	meta := clip.meta()
	meta.Clipper = "node-a"

	goodMeta := part{api.PartMetadata, mustJSON(t, meta)}
	goodTranscript := part{api.PartTranscript, mustJSON(t, transcript)}
	goodClip := part{api.PartClip, clip.data}

	noClipper := meta
	noClipper.Clipper = ""
	bigMeta := meta
	bigMeta.ToolVersions = map[string]string{"padding": strings.Repeat("x", MaxMetadataBytes)}
	bigTranscript := api.Transcript{Language: "en", Words: []api.Word{{Text: strings.Repeat("x", MaxTranscriptBytes)}}}

	tests := []struct {
		name  string
		parts []part
		want  api.ErrorCode
	}{
		{"metadata not JSON", []part{{api.PartMetadata, []byte("{nope")}, goodTranscript, goodClip}, api.CodeInvalidRequest},
		{"metadata with trailing data", []part{{api.PartMetadata, append(mustJSON(t, meta), " {}"...)}, goodTranscript, goodClip}, api.CodeInvalidRequest},
		{"metadata without clipper", []part{{api.PartMetadata, mustJSON(t, noClipper)}, goodTranscript, goodClip}, api.CodeInvalidRequest},
		{"metadata null", []part{{api.PartMetadata, []byte("null")}, goodTranscript, goodClip}, api.CodeInvalidRequest},
		{"transcript not JSON", []part{goodMeta, {api.PartTranscript, []byte(`{"words": [}`)}, goodClip}, api.CodeInvalidRequest},
		{"transcript words null", []part{goodMeta, {api.PartTranscript, []byte(`{"language": "en", "words": null}`)}, goodClip}, api.CodeInvalidRequest},
		{"transcript words missing", []part{goodMeta, {api.PartTranscript, []byte(`{"language": "en"}`)}, goodClip}, api.CodeInvalidRequest},
		{"transcript word ends before it starts", []part{goodMeta,
			{api.PartTranscript, []byte(`{"words": [{"text": "a", "start_seconds": 2, "end_seconds": 1}]}`)}, goodClip}, api.CodeInvalidRequest},
		{"transcript word with negative start", []part{goodMeta,
			{api.PartTranscript, []byte(`{"words": [{"text": "a", "start_seconds": -1, "end_seconds": 1}]}`)}, goodClip}, api.CodeInvalidRequest},
		{"wrong order", []part{goodTranscript, goodMeta, goodClip}, api.CodeInvalidRequest},
		{"clip before transcript", []part{goodMeta, goodClip, goodTranscript}, api.CodeInvalidRequest},
		{"clip missing", []part{goodMeta, goodTranscript}, api.CodeInvalidRequest},
		{"no parts", nil, api.CodeInvalidRequest},
		{"clip part misnamed", []part{goodMeta, goodTranscript, {"video", clip.data}}, api.CodeInvalidRequest},
		{"part after the clip", []part{goodMeta, goodTranscript, goodClip, {"extra", []byte("x")}}, api.CodeInvalidRequest},
		{"metadata too large", []part{{api.PartMetadata, mustJSON(t, bigMeta)}, goodTranscript, goodClip}, api.CodePayloadTooLarge},
		{"transcript too large", []part{goodMeta, {api.PartTranscript, mustJSON(t, bigTranscript)}, goodClip}, api.CodePayloadTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, 1<<20)
			jobID := f.leased("node-a")
			body, ct := multipartBody(t, tt.parts)
			status, code := f.post(jobID, body, ct)
			if code != tt.want || status != tt.want.HTTPStatus() {
				t.Fatalf("answer = %d %q, want %d %q", status, code, tt.want.HTTPStatus(), tt.want)
			}
			f.assertNothingStored(jobID)
			if j := f.job(jobID); j.Status != store.JobLeased {
				t.Errorf("job status = %s, want leased", j.Status)
			}
		})
	}
}

func TestHandlerNotMultipart(t *testing.T) {
	f := newFixture(t, 1<<20)
	jobID := f.leased("node-a")
	status, code := f.post(jobID, []byte(`{"clipper": "node-a"}`), "application/json")
	if status != http.StatusBadRequest || code != api.CodeInvalidRequest {
		t.Fatalf("answer = %d %q, want 400 invalid_request", status, code)
	}
	f.assertNothingStored(jobID)
}

// An upload that stops in the middle of the clip stores nothing.
func TestHandlerTruncatedUpload(t *testing.T) {
	f := newFixture(t, 1<<20)
	jobID := f.leased("node-a")
	clip := newClip(t, 100<<10)
	meta := clip.meta()
	meta.Clipper = "node-a"
	body, ct := multipartBody(t, []part{
		{api.PartMetadata, mustJSON(t, meta)},
		{api.PartTranscript, mustJSON(t, transcript)},
		{api.PartClip, clip.data},
	})
	for _, cut := range []int{len(body) - 50<<10, len(body) - 10} {
		status, code := f.post(jobID, body[:cut], ct)
		if status != http.StatusBadRequest || code != api.CodeInvalidRequest {
			t.Fatalf("cut at %d of %d: answer = %d %q, want 400 invalid_request", cut, len(body), status, code)
		}
		f.assertNothingStored(jobID)
	}

	// The whole body is accepted, so the cut was the only problem.
	status, code := f.post(jobID, body, ct)
	if status != http.StatusOK || code != "" {
		t.Fatalf("full upload: answer = %d %q", status, code)
	}
}

func TestHandlerRequiresSecret(t *testing.T) {
	f := newFixture(t, 1<<20)
	jobID := f.leased("node-a")
	req, err := http.NewRequest(http.MethodPost, f.server.URL+api.PathComplete(queue.JobID(jobID)), strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	f.assertNothingStored(jobID)
}

func TestCode(t *testing.T) {
	for err, want := range map[error]api.ErrorCode{
		fmt.Errorf("x: %w", ErrInvalid):          api.CodeInvalidRequest,
		fmt.Errorf("x: %w", ErrTooLarge):         api.CodePayloadTooLarge,
		fmt.Errorf("x: %w", ErrChecksumMismatch): api.CodeChecksumMismatch,
		fmt.Errorf("x: %w", queue.ErrUnknownJob): api.CodeUnknownJob,
		fmt.Errorf("disk full"):                  api.CodeInternal,
	} {
		if got := Code(err); got != want {
			t.Errorf("Code(%v) = %q, want %q", err, got, want)
		}
	}
}
