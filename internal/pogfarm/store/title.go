package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MaxTitleOverrideLength is the most characters (runes) a title override
// may have. The database refuses a longer one.
const MaxTitleOverrideLength = 1000

// ErrNotPending is returned (wrapped) by SetTitleOverride when the clip has
// already been approved or rejected.
var ErrNotPending = errors.New("store: clip is no longer pending review")

// GetTitleOverride returns the title override of a clip, or "" when it has
// none.
func GetTitleOverride(ctx context.Context, q Querier, clipID int64) (string, error) {
	var title sql.NullString
	err := q.QueryRowContext(ctx, `SELECT title_override FROM clips WHERE id = ?`, clipID).Scan(&title)
	return title.String, wrap("get title override", err)
}

// SetTitleOverride stores the title override of a clip that is still
// pending review. An empty title removes the override. It is ErrNotFound
// when the clip does not exist and ErrNotPending when it has been decided:
// the title of a clip only changes before the clip is approved.
func SetTitleOverride(ctx context.Context, q Querier, clipID int64, title string) error {
	var arg any
	if title != "" {
		arg = title
	}
	res, err := q.ExecContext(ctx, `
		UPDATE clips SET title_override = ?, updated_at = ?
		WHERE id = ? AND review_status = ?`,
		arg, timeArg(time.Now()), clipID, ReviewPending)
	if err != nil {
		return wrap("set title override", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrap("set title override", err)
	}
	if n == 1 {
		return nil
	}
	var status ReviewStatus
	err = q.QueryRowContext(ctx, `SELECT review_status FROM clips WHERE id = ?`, clipID).Scan(&status)
	if err != nil {
		return wrap("set title override", err)
	}
	return fmt.Errorf("store: set title override of clip %d (%s): %w", clipID, status, ErrNotPending)
}

// GetClipByReviewMessage returns the clip whose review message is messageID
// in chatID. The UNIQUE (telegram_chat_id, telegram_message_id) of clips
// serves the query.
func GetClipByReviewMessage(ctx context.Context, q Querier, chatID, messageID int64) (Clip, error) {
	return getClip(ctx, q, "get clip by review message",
		`telegram_chat_id = ? AND telegram_message_id = ?`, chatID, messageID)
}
