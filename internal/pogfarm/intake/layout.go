package intake

import (
	"fmt"
	"path"
	"time"
)

// Directories inside the data directory.
const (
	// ClipsDir holds the accepted clip files, one subdirectory per month.
	ClipsDir = "clips"
	// TempDir holds uploads in flight. It is on the same file system as
	// ClipsDir, so moving a finished upload into place is an atomic rename.
	// Nothing in it is referenced by the database.
	TempDir = "tmp"
)

// ClipPath returns where the clip of a job is stored, relative to the data
// directory and with forward slashes, as clips.file_path holds it:
// clips/<YYYY-MM>/job-<job id>.mp4, where the month is that of at in UTC.
//
// A job has at most one clip, so the job id names the file uniquely.
func ClipPath(jobID int64, at time.Time) string {
	return path.Join(ClipsDir, at.UTC().Format("2006-01"), fmt.Sprintf("job-%d.mp4", jobID))
}
