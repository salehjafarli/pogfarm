package api

import "time"

// Version is the contract version. Every route lives under PathPrefix.
const (
	Version    = "v1"
	PathPrefix = "/" + Version
)

// Routes, as patterns for http.ServeMux. The {id} segment is a job ID.
const (
	RouteClaim     = "POST " + PathPrefix + "/jobs/claim"
	RouteHeartbeat = "POST " + PathPrefix + "/jobs/{id}/heartbeat"
	RouteComplete  = "POST " + PathPrefix + "/jobs/{id}/complete"
	RouteFail      = "POST " + PathPrefix + "/jobs/{id}/fail"
)

// PathClaim is the path of the claim operation.
const PathClaim = PathPrefix + "/jobs/claim"

// PathHeartbeat returns the heartbeat path for a job.
func PathHeartbeat(jobID string) string { return PathPrefix + "/jobs/" + jobID + "/heartbeat" }

// PathComplete returns the complete path for a job.
func PathComplete(jobID string) string { return PathPrefix + "/jobs/" + jobID + "/complete" }

// PathFail returns the fail path for a job.
func PathFail(jobID string) string { return PathPrefix + "/jobs/" + jobID + "/fail" }

// Platform names the service a VOD comes from. It is the only field whose
// value is source-specific; everything else in the contract is the same for
// every platform.
type Platform string

// PlatformTwitch is the only platform today.
const PlatformTwitch Platform = "twitch"

// Job is one render job: a time range of one VOD to turn into a clip.
type Job struct {
	// ID identifies the job. Clippers treat it as opaque and send it back
	// unchanged in the path of the other operations.
	ID string `json:"id"`
	// Attempt counts how many times the job has been handed out, starting
	// at 1.
	Attempt int `json:"attempt"`
	// Platform is the source of the VOD and selects the clipper's downloader.
	Platform Platform `json:"platform"`
	// VODID is the platform's own identifier of the VOD.
	VODID string `json:"vod_id"`
	// StartSeconds and EndSeconds are offsets from the start of the VOD.
	// EndSeconds is greater than StartSeconds.
	StartSeconds float64 `json:"start_seconds"`
	EndSeconds   float64 `json:"end_seconds"`
	// Streamer is whose VOD this is.
	Streamer Streamer `json:"streamer"`
	// LeaseExpiresAt is when pogfarm takes the job back unless a heartbeat
	// extends the lease first.
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	// HeartbeatIntervalSeconds is how often the clipper sends a heartbeat
	// while it holds the job.
	HeartbeatIntervalSeconds int `json:"heartbeat_interval_seconds"`
}

// HeartbeatInterval returns HeartbeatIntervalSeconds as a duration.
func (j Job) HeartbeatInterval() time.Duration {
	return time.Duration(j.HeartbeatIntervalSeconds) * time.Second
}

// Streamer identifies a streamer on the job's platform.
type Streamer struct {
	// Login is the platform's stable account name, as used in URLs.
	Login string `json:"login"`
	// DisplayName is the name shown to viewers.
	DisplayName string `json:"display_name"`
}
