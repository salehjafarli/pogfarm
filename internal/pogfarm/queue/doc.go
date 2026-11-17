// Package queue is pogfarm's job queue. There is no separate queue service:
// the queue is the jobs table of package store, and this package holds the
// rules for moving a job between its states.
//
//	queued --Claim--> leased --Fail (retryable, attempts remain)--> queued
//	                         --Fail (otherwise)-------------------> failed
//	                         --Reap (lease expired, attempts remain)--> queued
//	                         --Reap (lease expired, last attempt)-----> failed
//
// While it works, the holder keeps the lease with Heartbeat. Reap, which
// RunReaper calls on a timer, takes back the jobs whose holder stopped
// sending heartbeats. Clippers lists the clippers the queue has seen.
//
// Every change is one write transaction on the store's single write
// connection, so changes happen one after another and two clippers can never
// be handed the same job.
//
// The package knows nothing about HTTP. The server's handlers call it and
// translate its errors (ErrNoJob, ErrUnknownJob, ErrLeaseLost,
// ErrAlreadyCompleted, ErrNoClipper) into the answers of the job contract.
//
// A job becomes done when its result arrives; that happens in package
// intake, which marks the job done in the same transaction that stores the
// clip.
package queue
