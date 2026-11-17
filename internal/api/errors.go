package api

import "net/http"

// ErrorCode is the machine-readable reason of a failed call. Clients decide
// what to do from the code, never from the message.
type ErrorCode string

const (
	// CodeUnauthenticated: the shared secret is missing or wrong.
	CodeUnauthenticated ErrorCode = "unauthenticated"
	// CodeInvalidRequest: the request is malformed or a field is invalid.
	CodeInvalidRequest ErrorCode = "invalid_request"
	// CodeUnknownJob: no job has this ID.
	CodeUnknownJob ErrorCode = "unknown_job"
	// CodeLeaseLost: the caller no longer holds the job. It abandons the
	// work.
	CodeLeaseLost ErrorCode = "lease_lost"
	// CodeAlreadyCompleted: the job already has a result. Returned by
	// heartbeat and fail; complete reports it as a successful
	// ResultAlreadyCompleted instead.
	CodeAlreadyCompleted ErrorCode = "already_completed"
	// CodePayloadTooLarge: a part of the upload exceeds pogfarm's limit.
	CodePayloadTooLarge ErrorCode = "payload_too_large"
	// CodeChecksumMismatch: the clip received does not match the SHA-256 in
	// the metadata. The upload can be retried.
	CodeChecksumMismatch ErrorCode = "checksum_mismatch"
	// CodeInternal: pogfarm failed. The call can be retried.
	CodeInternal ErrorCode = "internal"
)

// HTTPStatus returns the status code a response with this error code carries.
// Unknown codes map to 500.
func (c ErrorCode) HTTPStatus() int {
	switch c {
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeInvalidRequest:
		return http.StatusBadRequest
	case CodeUnknownJob:
		return http.StatusNotFound
	case CodeLeaseLost, CodeAlreadyCompleted:
		return http.StatusConflict
	case CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeChecksumMismatch:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

// ErrorResponse is the body of every response with a 4xx or 5xx status.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody describes one error.
type ErrorBody struct {
	Code ErrorCode `json:"code"`
	// Message is for people reading logs. It never contains secrets.
	Message string `json:"message"`
}

// NewErrorResponse builds an error body.
func NewErrorResponse(code ErrorCode, message string) ErrorResponse {
	return ErrorResponse{Error: ErrorBody{Code: code, Message: message}}
}
