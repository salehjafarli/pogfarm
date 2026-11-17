package review

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// Errors of Resend.
var (
	// ErrClipNotFound is returned for a clip id that is not in the store.
	ErrClipNotFound = errors.New("review: no such clip")
	// ErrNotPending is returned for a clip that is no longer awaiting
	// review.
	ErrNotPending = errors.New("review: clip is not awaiting review")
)

// Resend sends the review message of a clip awaiting review again, whether
// or not it was sent before, and records the new message on the clip in
// place of the old one. The old message stays in the chat; its buttons
// carry the same clip id.
//
// The message is the one the sender would send: the video with its caption
// and buttons, or a text message when the file is missing, too large, or
// refused by Telegram with a client error. A clip that is gone is
// ErrClipNotFound; one that has been decided, before or while it was being
// sent, is ErrNotPending.
//
// Resend reads no state that Run or SendPending changes, so the bot may call
// it while the sender runs on another goroutine. If the sender sends the
// clip's first message at the same moment, the owner sees both; the one
// recorded last is kept on the clip.
func (s *Sender) Resend(ctx context.Context, clipID int64) (*Message, error) {
	info, file, err := s.resendLoad(ctx, clipID)
	if err != nil {
		return nil, err
	}
	msg, err := s.resendOne(ctx, info, file)
	if err != nil {
		return nil, fmt.Errorf("review: resend clip %d: %w", clipID, err)
	}
	chatID := msg.Chat.ID
	if chatID == 0 {
		chatID = s.chatID
	}
	var recorded bool
	err = s.store.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE clips SET telegram_chat_id = ?, telegram_message_id = ?, updated_at = ?
			WHERE id = ? AND review_status = ?`,
			chatID, msg.MessageID, store.FormatTime(s.now()), clipID, store.ReviewPending)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		recorded = n == 1
		return err
	})
	if err != nil {
		return msg, fmt.Errorf("review: record resent message of clip %d: %w", clipID, err)
	}
	if !recorded {
		s.log.Warn("review: clip was decided while its review message was resent; the new one is not recorded",
			"clip", clipID, "message_id", msg.MessageID)
		return msg, fmt.Errorf("%w: clip %d", ErrNotPending, clipID)
	}
	s.log.Info("review: review message resent", "clip", clipID, "message_id", msg.MessageID)
	return msg, nil
}

// resendOne sends the review message of one clip. Unlike sendOne it keeps no
// retry state: a video Telegram refuses with a client error falls back to a
// text message at once.
func (s *Sender) resendOne(ctx context.Context, info clipInfo, file string) (*Message, error) {
	const missing = "The clip file is missing, so the video cannot be shown. It cannot be posted; reject it to close it."
	st, statErr := os.Stat(file)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		msg, _, err := s.sendText(ctx, info, rejectKeyboard(info.ClipID), missing)
		return msg, err
	case statErr != nil:
		return nil, statErr
	case st.Size() > s.MaxUploadBytes:
		msg, _, err := s.sendText(ctx, info, ReviewKeyboard(info.ClipID), fmt.Sprintf(
			"The clip is too large to send through Telegram (%s; bots may upload %s). Review the file on the server: %s",
			formatMB(st.Size()), formatMB(s.MaxUploadBytes), file))
		return msg, err
	}
	msg, err := s.client.SendVideo(ctx, s.chatID, VideoUpload{
		Path:     file,
		Caption:  info.caption(MaxCaptionLength),
		Duration: int(info.DurationSeconds + 0.5),
		Width:    info.width,
		Height:   info.height,
		Markup:   ReviewKeyboard(info.ClipID),
	})
	if errors.Is(err, fs.ErrNotExist) {
		msg, _, err = s.sendText(ctx, info, rejectKeyboard(info.ClipID), missing)
		return msg, err
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code >= 400 && apiErr.Code < 500 && apiErr.Code != http.StatusTooManyRequests {
		msg, _, err = s.sendText(ctx, info, ReviewKeyboard(info.ClipID),
			"Telegram refused the video. Review the file on the server: "+file)
	}
	return msg, err
}

// resendLoad reads what the review message of a clip shows and the path of
// its file, for a clip awaiting review whether or not it was sent.
func (s *Sender) resendLoad(ctx context.Context, id int64) (info clipInfo, file string, err error) {
	var (
		status  store.ReviewStatus
		path    string
		display string
		login   string
	)
	info.ClipID = id
	err = s.store.Reader().QueryRowContext(ctx, `
		SELECT c.review_status, c.file_path, c.duration_seconds, c.width, c.height,
			c.transcript_json, cand.start_seconds, v.title, s.display_name, s.login
		FROM clips c
		JOIN jobs j ON j.id = c.job_id
		JOIN candidates cand ON cand.id = j.candidate_id
		JOIN vods v ON v.id = cand.vod_id
		JOIN streamers s ON s.id = v.streamer_id
		WHERE c.id = ?`, id).Scan(
		&status, &path, &info.DurationSeconds, &info.width, &info.height,
		&info.TranscriptJSON, &info.OffsetSeconds, &info.VODTitle, &display, &login)
	if errors.Is(err, sql.ErrNoRows) {
		return info, "", fmt.Errorf("%w: %d", ErrClipNotFound, id)
	}
	if err != nil {
		return info, "", fmt.Errorf("review: load clip %d: %w", id, err)
	}
	if status != store.ReviewPending {
		return info, "", fmt.Errorf("%w: clip %d is %s", ErrNotPending, id, status)
	}
	info.Streamer = display
	if clean(display) == "" {
		info.Streamer = login
	}
	return info, filepath.Join(s.dataDir, filepath.FromSlash(path)), nil
}
