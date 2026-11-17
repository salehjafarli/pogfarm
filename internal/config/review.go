package config

import (
	"fmt"
	"time"
)

// MaxUndoWindow is the longest undo window. It equals MinCleanupRetention,
// so the file of a rejected clip is never deleted while the rejection can
// still be undone.
const MaxUndoWindow = MinCleanupRetention

// Review is the "review" section of the main service's configuration: what
// happens when the owner taps Approve or Reject on a review message.
type Review struct {
	// UndoWindow is how long after a decision it can be undone, as long as
	// no upload of the clip has started.
	UndoWindow time.Duration `yaml:"undo_window" env:"POGFARM_REVIEW_UNDO_WINDOW"`
}

// defaultReview is the review section when nothing is configured.
func defaultReview() Review {
	return Review{UndoWindow: 5 * time.Minute}
}

func (r Review) validate() []string {
	if r.UndoWindow <= 0 || r.UndoWindow > MaxUndoWindow {
		return []string{fmt.Sprintf(
			"pogfarm.review.undo_window (POGFARM_REVIEW_UNDO_WINDOW) must be a positive duration of at most %s: got %s",
			MaxUndoWindow, r.UndoWindow)}
	}
	return nil
}
