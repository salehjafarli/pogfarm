package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Errors a Client call can be matched against with errors.Is. Each of the
// first eight stands for one error code of the contract.
var (
	// ErrUnauthorized: the shared secret is missing or wrong
	// (CodeUnauthenticated). Retrying with the same secret cannot succeed.
	ErrUnauthorized = errors.New("api: unauthorized")
	// ErrInvalidRequest: pogfarm found the request malformed
	// (CodeInvalidRequest).
	ErrInvalidRequest = errors.New("api: invalid request")
	// ErrUnknownJob: no job has this ID (CodeUnknownJob).
	ErrUnknownJob = errors.New("api: unknown job")
	// ErrLeaseLost: the caller no longer holds the job (CodeLeaseLost).
	ErrLeaseLost = errors.New("api: lease lost")
	// ErrAlreadyCompleted: the job already has a result
	// (CodeAlreadyCompleted). Returned by Heartbeat and Fail; Complete
	// reports it as the successful ResultAlreadyCompleted instead.
	ErrAlreadyCompleted = errors.New("api: job already completed")
	// ErrPayloadTooLarge: a part of the upload exceeds pogfarm's limit
	// (CodePayloadTooLarge).
	ErrPayloadTooLarge = errors.New("api: payload too large")
	// ErrChecksumMismatch: the clip pogfarm received does not match the
	// SHA-256 in the metadata (CodeChecksumMismatch). The upload can be
	// sent again.
	ErrChecksumMismatch = errors.New("api: checksum mismatch")
	// ErrInternal: pogfarm failed (CodeInternal). Returned once the
	// client's own retries are used up.
	ErrInternal = errors.New("api: pogfarm internal error")

	// ErrNoJob: Claim found nothing to do. The error is a *NoJobError.
	ErrNoJob = errors.New("api: no job available")
	// ErrBadResponse: pogfarm answered with a success status but a body the
	// client cannot use.
	ErrBadResponse = errors.New("api: bad response")
)

// codeErrors maps every error code of the contract to its Err variable.
var codeErrors = map[ErrorCode]error{
	CodeUnauthenticated:  ErrUnauthorized,
	CodeInvalidRequest:   ErrInvalidRequest,
	CodeUnknownJob:       ErrUnknownJob,
	CodeLeaseLost:        ErrLeaseLost,
	CodeAlreadyCompleted: ErrAlreadyCompleted,
	CodePayloadTooLarge:  ErrPayloadTooLarge,
	CodeChecksumMismatch: ErrChecksumMismatch,
	CodeInternal:         ErrInternal,
}

// Error is an answer from pogfarm with a status outside 2xx. It matches the
// Err variable of its Code with errors.Is:
//
//	if errors.Is(err, api.ErrLeaseLost) { ... }
type Error struct {
	// Status is the HTTP status of the response.
	Status int
	// Code is the contract's error code. It is empty when the body was not
	// the contract's error document, for example an answer from a proxy,
	// and may be a code this client does not know.
	Code ErrorCode
	// Message is pogfarm's explanation, for logs only.
	Message string
}

// newError builds the error for a response with a non-2xx status.
func newError(status int, body []byte) *Error {
	e := &Error{Status: status}
	var resp ErrorResponse
	if json.Unmarshal(body, &resp) == nil {
		e.Code = resp.Error.Code
		e.Message = resp.Error.Message
	}
	return e
}

func (e *Error) Error() string {
	s := fmt.Sprintf("pogfarm answered %d", e.Status)
	if e.Code != "" {
		s += " " + string(e.Code)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Is reports whether target is the Err variable of e's code.
func (e *Error) Is(target error) bool {
	sentinel, ok := codeErrors[e.Code]
	return ok && sentinel == target
}

// Transient reports whether the same call could succeed later. As the
// contract says, that is decided by the status, so it also covers codes
// this client does not know: 5xx is transient, everything else is not.
func (e *Error) Transient() bool { return e.Status >= 500 }

// NoJobError is returned by Claim when pogfarm has nothing for this clipper.
// It matches ErrNoJob.
type NoJobError struct {
	// RetryAfter is how long pogfarm asks the clipper to wait before it
	// claims again.
	RetryAfter time.Duration
}

func (e *NoJobError) Error() string {
	return fmt.Sprintf("%v, retry after %v", ErrNoJob, e.RetryAfter)
}

// Is reports whether target is ErrNoJob.
func (e *NoJobError) Is(target error) bool { return target == ErrNoJob }
