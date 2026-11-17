package api

import "time"

// ClaimRequest asks pogfarm for one job.
type ClaimRequest struct {
	// Clipper is the name of the calling clipper. It identifies the lease
	// holder in every later call for the job.
	Clipper string `json:"clipper"`
}

// ClaimResponse carries the job handed out, or none.
type ClaimResponse struct {
	// Job is null when there is nothing to do.
	Job *Job `json:"job"`
	// RetryAfterSeconds is set when Job is null: how long the clipper waits
	// before it asks again.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

// HeartbeatRequest extends the lease of a job the caller holds.
type HeartbeatRequest struct {
	Clipper string `json:"clipper"`
}

// HeartbeatResponse is the answer to a heartbeat that kept the lease.
type HeartbeatResponse struct {
	// LeaseExpiresAt is the new expiry.
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// FailRequest reports that the caller could not finish a job.
type FailRequest struct {
	Clipper string `json:"clipper"`
	// Reason is a short human-readable explanation, shown in status output.
	Reason string `json:"reason"`
	// Retryable says whether another attempt could succeed. A deleted VOD is
	// not retryable; a network error or a shutdown is.
	Retryable bool `json:"retryable"`
}

// Names of the multipart form parts of the complete operation. They are sent
// in this order, so pogfarm can check the small parts before it reads the
// clip.
const (
	PartMetadata   = "metadata"
	PartTranscript = "transcript"
	PartClip       = "clip"
)

// ClipContentType is the content type of the clip part.
const ClipContentType = "video/mp4"

// CompleteMetadata is the JSON document in the metadata part.
type CompleteMetadata struct {
	Clipper string `json:"clipper"`
	// SHA256 is the lowercase hex SHA-256 of the clip part's bytes.
	SHA256 string `json:"sha256"`
	// SizeBytes is the size of the clip part.
	SizeBytes int64 `json:"size_bytes"`
	// DurationSeconds is the length of the rendered clip.
	DurationSeconds float64 `json:"duration_seconds"`
	// Width and Height are the clip's resolution in pixels.
	Width  int `json:"width"`
	Height int `json:"height"`
	// ToolVersions records the tools that produced the clip, keyed by tool
	// name, for example "ffmpeg". It is informational.
	ToolVersions map[string]string `json:"tool_versions,omitempty"`
}

// Transcript is the JSON document in the transcript part: what is said in the
// clip, word by word.
type Transcript struct {
	// Language is the spoken language as a BCP 47 tag such as "en". It is
	// empty when unknown.
	Language string `json:"language"`
	// Words are in speaking order. A clip without speech has none.
	Words []Word `json:"words"`
}

// Word is one spoken word and when it is said, as offsets from the start of
// the clip.
type Word struct {
	Text         string  `json:"text"`
	StartSeconds float64 `json:"start_seconds"`
	EndSeconds   float64 `json:"end_seconds"`
}

// CompleteResult says what pogfarm did with an uploaded result.
type CompleteResult string

const (
	// ResultAccepted means this upload became the job's result.
	ResultAccepted CompleteResult = "accepted"
	// ResultAlreadyCompleted means the job already had a result and this
	// upload was discarded. The clipper treats it as success.
	ResultAlreadyCompleted CompleteResult = "already_completed"
)

// CompleteResponse is the answer to an upload that pogfarm processed.
type CompleteResponse struct {
	Result CompleteResult `json:"result"`
}
