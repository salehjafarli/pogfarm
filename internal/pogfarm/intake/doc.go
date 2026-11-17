// Package intake receives the results of render jobs: the complete operation
// of the job contract.
//
// A result is a finished clip, its transcript and its metadata. Intake
// streams the clip to a temporary file inside the data directory while it
// computes the SHA-256, checks size and checksum against the metadata, moves
// the file into place with a rename and then, in one short write
// transaction, stores the clip row (review status pending) and marks the job
// done. File I/O never happens inside the transaction.
//
// First result wins. The first valid result for a job becomes its result,
// whoever sends it: holding the lease is not required, and a job that was
// queued again, is held by another clipper or was marked failed still
// accepts its first result. Every later result is discarded, its file is
// never moved into place, and it is answered with
// api.ResultAlreadyCompleted.
//
// Crash safety. The clip file is synced and renamed into place, and its
// directory synced, before the transaction that records it commits, so the
// database never points at a file that is not there. A crash after the
// rename and before the commit leaves a file under ClipsDir with no row (an
// orphan): the job is unchanged, a later upload for it is accepted as
// usual, and the orphan is removed by the file cleanup, which deletes files
// that no row refers to. A crash before the rename leaves a file in TempDir,
// which is emptied on start.
//
// File cleanup. A Cleaner (Intake.NewCleaner) owns the lifetime of the
// files: EmptyTemp empties TempDir at startup, and Sweep, which Run calls
// on a timer, deletes the files of rejected clips and of clips posted to
// every target once their retention is over, recording file_removed_at,
// and deletes orphans. ClipPath is the one place clip paths are built.
//
// Intake serves one data directory and there is one Intake per process;
// pogfarm itself runs exactly once.
package intake
