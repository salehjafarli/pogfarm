package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
)

// errInvalidBody marks a JSON request body that cannot be used.
var errInvalidBody = errors.New("invalid request body")

// internalMessage is the message of every internal error. The error itself
// is logged, never sent, because it may describe pogfarm's internals.
const internalMessage = "internal error"

// claim is POST /v1/jobs/claim.
func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var req api.ClaimRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, "claim", err)
		return
	}
	job, err := s.queue.Claim(r.Context(), req.Clipper)
	if errors.Is(err, queue.ErrNoJob) {
		writeJSON(w, http.StatusOK, api.ClaimResponse{RetryAfterSeconds: s.retryAfter})
		return
	}
	if err != nil {
		s.writeError(w, r, "claim", err)
		return
	}
	s.logger.Info("job claimed", "job", job.ID, "attempt", job.Attempt, "clipper", req.Clipper)
	writeJSON(w, http.StatusOK, api.ClaimResponse{Job: &job})
}

// heartbeat is POST /v1/jobs/{id}/heartbeat.
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	id, err := queue.ParseJobID(r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, "heartbeat", err)
		return
	}
	var req api.HeartbeatRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, "heartbeat", err)
		return
	}
	expires, err := s.queue.Heartbeat(r.Context(), id, req.Clipper)
	if err != nil {
		s.writeError(w, r, "heartbeat", err)
		return
	}
	writeJSON(w, http.StatusOK, api.HeartbeatResponse{LeaseExpiresAt: expires})
}

// fail is POST /v1/jobs/{id}/fail.
func (s *Server) fail(w http.ResponseWriter, r *http.Request) {
	id, err := queue.ParseJobID(r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, "fail", err)
		return
	}
	var req api.FailRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, "fail", err)
		return
	}
	job, err := s.queue.Fail(r.Context(), id, req.Clipper, req.Reason, req.Retryable)
	if err != nil {
		s.writeError(w, r, "fail", err)
		return
	}
	s.logger.Info("job failed by clipper", "job", job.ID, "clipper", req.Clipper,
		"retryable", req.Retryable, "status", job.Status)
	w.WriteHeader(http.StatusNoContent)
}

// health is GET /healthz: 200 when the database answers, 503 otherwise.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	var one int
	if err := s.store.Reader().QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		s.logger.Error("health check: database unreachable", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// healthResponse is the body of the health endpoint.
type healthResponse struct {
	Status string `json:"status"`
}

// decodeJSON reads a JSON request body of at most MaxJSONBodyBytes into v.
// Unknown fields are ignored, as the contract requires.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxJSONBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return fmt.Errorf("%w: body exceeds %d bytes", errInvalidBody, tooLarge.Limit)
		}
		return fmt.Errorf("%w: %v", errInvalidBody, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%w: %v", errInvalidBody, err)
	}
	return nil
}

// errorCode maps an error of the queue or of decoding to the contract's
// error code. Anything unexpected is internal.
func errorCode(err error) api.ErrorCode {
	switch {
	case errors.Is(err, errInvalidBody), errors.Is(err, queue.ErrNoClipper):
		return api.CodeInvalidRequest
	case errors.Is(err, queue.ErrUnknownJob):
		return api.CodeUnknownJob
	case errors.Is(err, queue.ErrLeaseLost):
		return api.CodeLeaseLost
	case errors.Is(err, queue.ErrAlreadyCompleted):
		return api.CodeAlreadyCompleted
	default:
		return api.CodeInternal
	}
}

// writeError answers with the contract's error body for err. An internal
// error is logged and answered with a fixed message.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, op string, err error) {
	code := errorCode(err)
	msg := err.Error()
	if code == api.CodeInternal {
		s.logger.Error("job API: "+op, "job", r.PathValue("id"), "err", err)
		msg = internalMessage
	}
	writeJSON(w, code.HTTPStatus(), api.NewErrorResponse(code, msg))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
