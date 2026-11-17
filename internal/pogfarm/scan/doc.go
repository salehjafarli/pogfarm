// Package scan finds the VODs of the allowlisted streamers and the moments
// of a VOD that are worth turning into clips, and queues their render jobs.
//
// Everything in it works on the common types of package source, so it knows
// nothing about any particular platform.
//
// The detection logic (DetectSpikes, BuildCandidates) is pure: it does no
// I/O, reads no clock and returns the same output for the same input. The
// rest talks to the sources and the store:
//
//   - Discoverer records the new VODs of the allowlist. Discover is one
//     pass; Run syncs the allowlist into the store once and then runs a pass
//     on an interval.
//   - QueueCandidates stores the candidates of a VOD and enqueues their
//     render jobs in one transaction.
//   - Scanner takes each recorded VOD through chat fetch, viewer clips,
//     detection and QueueCandidates, retrying failed scans with backoff.
//     ScanNext is one scan; Run scans in a background loop.
//
// The loops are meant to run in their own goroutines next to the bot and the
// job API. Their slow work (platform calls, chat downloads) happens outside
// every write transaction.
package scan
