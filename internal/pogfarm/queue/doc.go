// Package queue is pogfarm's job queue. There is no separate queue service:
// the queue is the jobs table of package store, and this package holds the
// rules for moving a job between its states.
//
//	queued --Claim--> leased --Fail (retryable, attempts remain)--> queued
//	                         --Fail (otherwise)-------------------> failed
//
// Every change is one write transaction on the store's single write
// connection, so changes happen one after another and two clippers can never
// be handed the same job.
//
// The package knows nothing about HTTP. The server's handlers call it and
// translate its errors (ErrNoJob, ErrUnknownJob, ErrLeaseLost,
// ErrAlreadyCompleted, ErrNoClipper) into the answers of the job contract.
//
// Heartbeats and the reaper that takes back expired leases, and the intake of
// results that marks a job done, are not here yet; they build on the same
// Queue.
package queue
