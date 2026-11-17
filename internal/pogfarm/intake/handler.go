package intake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
)

// envelopeBytes is what the multipart framing (boundaries and part headers)
// may add to the parts themselves.
const envelopeBytes = 64 << 10

// ServeHTTP is the complete operation of the job contract,
// POST /v1/jobs/{id}/complete. Register it under api.RouteComplete, so that
// the job id is the request's "id" path value, and behind
// api.RequireSecret:
//
//	mux.Handle(api.RouteComplete, in)
//
// It reads the multipart body as a stream, in the order the contract
// prescribes: the metadata and transcript parts are checked before the clip
// part is read, and the clip goes straight to disk through Complete. The
// whole body is capped at the sum of the part limits, so an oversize upload
// is cut off instead of read to its end.
func (in *Intake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	result, err := in.serve(w, r)
	if err != nil {
		code := Code(err)
		msg := err.Error()
		if code == api.CodeInternal {
			in.logger.Error("intake: complete", "job", r.PathValue("id"), "err", err)
			msg = "internal error"
		}
		writeJSON(w, code.HTTPStatus(), api.NewErrorResponse(code, msg))
		return
	}
	writeJSON(w, http.StatusOK, api.CompleteResponse{Result: result})
}

func (in *Intake) serve(w http.ResponseWriter, r *http.Request) (api.CompleteResult, error) {
	jobID, err := queue.ParseJobID(r.PathValue("id"))
	if err != nil {
		return "", err
	}
	r.Body = http.MaxBytesReader(w, r.Body, in.maxClip+MaxMetadataBytes+MaxTranscriptBytes+envelopeBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		return "", fmt.Errorf("%w: not a multipart/form-data request: %v", ErrInvalid, err)
	}

	var meta api.CompleteMetadata
	if err := readJSONPart(mr, api.PartMetadata, MaxMetadataBytes, &meta); err != nil {
		return "", err
	}
	var transcript api.Transcript
	if err := readJSONPart(mr, api.PartTranscript, MaxTranscriptBytes, &transcript); err != nil {
		return "", err
	}
	clip, err := nextPart(mr, api.PartClip)
	if err != nil {
		return "", err
	}
	return in.Complete(r.Context(), jobID, meta, transcript, &clipReader{part: clip, mr: mr})
}

// nextPart returns the next part, which must be the one called name.
func nextPart(mr *multipart.Reader, name string) (*multipart.Part, error) {
	p, err := mr.NextPart()
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: part %q is missing", ErrInvalid, name)
	}
	if err != nil {
		return nil, bodyError(err, "part "+name)
	}
	if got := p.FormName(); got != name {
		return nil, fmt.Errorf("%w: expected part %q, got %q", ErrInvalid, name, got)
	}
	return p, nil
}

// readJSONPart reads the next part, which must be called name and hold at
// most limit bytes of JSON, into v.
func readJSONPart(mr *multipart.Reader, name string, limit int64, v any) error {
	p, err := nextPart(mr, name)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(p, limit+1))
	if err != nil {
		return bodyError(err, "part "+name)
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("%w: part %q exceeds the limit of %d bytes", ErrTooLarge, name, limit)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%w: part %q: %v", ErrInvalid, name, err)
	}
	return nil
}

// clipReader reads the clip part. At its end it checks that no part
// follows, so a malformed upload fails before its result is stored.
type clipReader struct {
	part *multipart.Part
	mr   *multipart.Reader
}

func (c *clipReader) Read(p []byte) (int, error) {
	n, err := c.part.Read(p)
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, io.EOF) {
		return n, bodyError(err, "part "+api.PartClip)
	}
	switch _, err := c.mr.NextPart(); {
	case errors.Is(err, io.EOF):
		return n, io.EOF
	case err == nil:
		return n, fmt.Errorf("%w: a part follows the %q part", ErrInvalid, api.PartClip)
	default:
		return n, bodyError(err, "end of the upload")
	}
}

// bodyError classifies an error from reading the request body: the body
// limit is ErrTooLarge, anything else an upload that ended early or is
// malformed.
func bodyError(err error, what string) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return fmt.Errorf("%w: the upload exceeds %d bytes", ErrTooLarge, tooLarge.Limit)
	}
	return fmt.Errorf("%w: %s: %v", ErrInvalid, what, err)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
