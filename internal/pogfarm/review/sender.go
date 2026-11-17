package review

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// Defaults of the sender.
const (
	// DefaultSendInterval is the least time between two review messages.
	// Telegram asks bots to stay around one message a second in a chat;
	// review messages are rare, so the margin costs nothing.
	DefaultSendInterval = 3 * time.Second
	// DefaultSendPollInterval is how often the sender looks for new clips.
	DefaultSendPollInterval = 10 * time.Second

	defaultSendMinBackoff = 30 * time.Second
	defaultSendMaxBackoff = 30 * time.Minute
	// maxVideoRefusals is how many times Telegram may refuse the video of a
	// clip with a client error before the clip is sent as a text message
	// instead, so a clip Telegram will not take still reaches the owner.
	maxVideoRefusals = 3
)

// SendResult says what one pass of the sender did. Every list holds clip ids
// in the order they were handled.
type SendResult struct {
	// Sent are the clips sent as a video with the review buttons.
	Sent []int64
	// Notified are the clips sent as a text message with the buttons
	// instead: the file is too large for a bot to upload, missing, or
	// refused by Telegram.
	Notified []int64
	// Deferred are the clips whose send failed and is retried by a later
	// pass after a backoff.
	Deferred []int64
	// RetryAfter is set when Telegram's flood control stopped the pass: no
	// message is sent before it has passed.
	RetryAfter time.Duration
}

// Sender posts clips awaiting review to the owner's chat: each pending clip
// that has no review message yet is sent once, as a video with its details
// and transcript in the caption and Approve and Reject buttons under it, and
// the message is recorded on the clip.
//
// Sending and recording are two steps, the network call outside any
// transaction. If pogfarm stops between them (after Telegram accepted the
// message, before the ids were written), the clip is still unsent in the
// database and is sent again after the restart: the owner then sees two
// review messages for one clip. Both carry the clip id in their buttons, so
// either can be used. A clip is never lost: it stays pending and unsent until
// a message for it has been recorded. If only the write fails, the sender
// keeps the message in memory and records it on a later pass without sending
// again.
//
// A Sender is used from one goroutine: Run, or calls to SendPending.
type Sender struct {
	client  *Client
	store   *store.Store
	chatID  int64
	dataDir string
	log     *slog.Logger

	// Interval is the least time between two messages.
	Interval time.Duration
	// PollInterval is how long Run waits between passes.
	PollInterval time.Duration
	// MaxUploadBytes is the largest file sent as a video.
	MaxUploadBytes int64
	// PostTitle, when set, returns the title a clip will be posted with,
	// which the review message then shows. An error is logged and the
	// message is sent without the title.
	PostTitle func(ctx context.Context, clipID int64) (string, error)

	minBackoff time.Duration
	maxBackoff time.Duration
	now        func() time.Time
	// sleep waits for d and reports whether it did so in full, as opposed
	// to being cut short by ctx. Tests replace it.
	sleep func(ctx context.Context, d time.Duration) bool

	lastSend    time.Time
	pausedUntil time.Time
	retries     map[int64]*retryState
	// unrecorded holds messages Telegram accepted whose ids could not be
	// written yet.
	unrecorded map[int64]*Message
}

type retryState struct {
	failures int
	// refusals counts client errors (4xx other than 429) on the video.
	refusals int
	next     time.Time
}

// NewSender returns a sender that posts to chatID through client. dataDir is
// the data directory clip paths are relative to.
func NewSender(client *Client, st *store.Store, chatID int64, dataDir string, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Sender{
		client:         client,
		store:          st,
		chatID:         chatID,
		dataDir:        dataDir,
		log:            logger,
		Interval:       DefaultSendInterval,
		PollInterval:   DefaultSendPollInterval,
		MaxUploadBytes: MaxUploadBytes,
		minBackoff:     defaultSendMinBackoff,
		maxBackoff:     defaultSendMaxBackoff,
		now:            time.Now,
		sleep:          sleep,
		retries:        map[int64]*retryState{},
		unrecorded:     map[int64]*Message{},
	}
}

// Run sends clips awaiting review until ctx is cancelled: one pass at once,
// then one every PollInterval, or once Telegram's flood control allows. A
// pass that fails is logged and retried. Run it in a goroutine of its own;
// it never blocks the bot.
func (s *Sender) Run(ctx context.Context) {
	for {
		res, err := s.SendPending(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.Error("review: send pass failed, will retry", "err", err)
		}
		wait := s.PollInterval
		if res.RetryAfter > 0 {
			wait = res.RetryAfter
		}
		if !s.sleep(ctx, wait) {
			return
		}
	}
}

// SendPending makes one pass: every clip that is pending review and has no
// review message is sent, oldest first, at most one message per Interval.
//
// A clip whose send fails is skipped for a backoff that doubles from 30
// seconds up to 30 minutes, and the pass goes on with the next clip, so one
// failing clip never holds up the others. A 429 from Telegram stops the pass;
// SendResult.RetryAfter says how long Telegram asked to wait, and no message
// is sent before then. The error is a database error or ctx's.
func (s *Sender) SendPending(ctx context.Context) (SendResult, error) {
	var res SendResult
	if wait := s.pausedUntil.Sub(s.now()); wait > 0 {
		res.RetryAfter = wait
		return res, nil
	}
	ids, err := s.unsent(ctx)
	if err != nil {
		return res, err
	}
	s.forget(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if msg := s.unrecorded[id]; msg != nil {
			if err := s.record(ctx, id, msg); err != nil {
				return res, err
			}
			delete(s.unrecorded, id)
			continue
		}
		if r := s.retries[id]; r != nil && s.now().Before(r.next) {
			continue
		}
		info, file, ok, err := s.load(ctx, id)
		if err != nil {
			return res, err
		}
		if !ok {
			// Decided or sent since the list was read.
			continue
		}
		if wait := s.lastSend.Add(s.Interval).Sub(s.now()); wait > 0 {
			if !s.sleep(ctx, wait) {
				return res, ctx.Err()
			}
		}
		msg, asVideo, err := s.sendOne(ctx, info, file)
		s.lastSend = s.now()
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Code == http.StatusTooManyRequests {
				wait := max(apiErr.RetryAfter, time.Second)
				s.pausedUntil = s.now().Add(wait)
				res.RetryAfter = wait
				s.log.Warn("review: telegram flood control, pausing sends", "clip", id, "retry_in", wait)
				return res, nil
			}
			wait := s.fail(id, err)
			res.Deferred = append(res.Deferred, id)
			s.log.Warn("review: sending clip failed, will retry", "clip", id, "err", err, "retry_in", wait)
			continue
		}
		delete(s.retries, id)
		if err := s.record(ctx, id, msg); err != nil {
			s.unrecorded[id] = msg
			return res, err
		}
		if asVideo {
			res.Sent = append(res.Sent, id)
			s.log.Info("review: clip sent for review", "clip", id, "message_id", msg.MessageID)
		} else {
			res.Notified = append(res.Notified, id)
			s.log.Info("review: clip sent for review as text", "clip", id, "message_id", msg.MessageID)
		}
	}
	return res, nil
}

// forget drops the retry state of clips that no longer wait to be sent.
func (s *Sender) forget(ids []int64) {
	waiting := make(map[int64]bool, len(ids))
	for _, id := range ids {
		waiting[id] = true
	}
	for id := range s.retries {
		if !waiting[id] {
			delete(s.retries, id)
		}
	}
	for id := range s.unrecorded {
		if !waiting[id] {
			delete(s.unrecorded, id)
		}
	}
}

// fail records a failed send of a clip and returns how long it is left
// alone.
func (s *Sender) fail(id int64, err error) time.Duration {
	r := s.retries[id]
	if r == nil {
		r = &retryState{}
		s.retries[id] = r
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code >= 400 && apiErr.Code < 500 {
		r.refusals++
	}
	wait := s.minBackoff
	for i := 0; i < r.failures && wait < s.maxBackoff; i++ {
		wait *= 2
	}
	wait = min(wait, s.maxBackoff)
	r.failures++
	r.next = s.now().Add(wait)
	return wait
}

// sendOne sends the review message of one clip: the video, or a text
// message when the file is too large, missing, or keeps being refused.
// asVideo says which.
func (s *Sender) sendOne(ctx context.Context, info clipInfo, file string) (msg *Message, asVideo bool, err error) {
	st, statErr := os.Stat(file)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return s.sendText(ctx, info, rejectKeyboard(info.ClipID),
			"The clip file is missing, so the video cannot be shown. It cannot be posted; reject it to close it.")
	case statErr != nil:
		// Possibly passing (a storage hiccup): retried after a backoff.
		return nil, false, statErr
	case st.Size() > s.MaxUploadBytes:
		return s.sendText(ctx, info, ReviewKeyboard(info.ClipID), fmt.Sprintf(
			"The clip is too large to send through Telegram (%s; bots may upload %s). Review the file on the server: %s",
			formatMB(st.Size()), formatMB(s.MaxUploadBytes), file))
	}
	if r := s.retries[info.ClipID]; r != nil && r.refusals >= maxVideoRefusals {
		return s.sendText(ctx, info, ReviewKeyboard(info.ClipID), fmt.Sprintf(
			"Telegram refused the video %d times. Review the file on the server: %s", r.refusals, file))
	}

	msg, err = s.client.SendVideo(ctx, s.chatID, VideoUpload{
		Path:     file,
		Caption:  info.caption(MaxCaptionLength),
		Duration: int(info.DurationSeconds + 0.5),
		Width:    info.width,
		Height:   info.height,
		Markup:   ReviewKeyboard(info.ClipID),
	})
	if errors.Is(err, fs.ErrNotExist) {
		// Removed between the check and the upload.
		return s.sendText(ctx, info, rejectKeyboard(info.ClipID),
			"The clip file is missing, so the video cannot be shown. It cannot be posted; reject it to close it.")
	}
	return msg, err == nil, err
}

func (s *Sender) sendText(ctx context.Context, info clipInfo, keyboard *InlineKeyboardMarkup, note string) (*Message, bool, error) {
	text := truncateText(info.header()+"\n\n"+note+"\n\n"+transcriptText(info.TranscriptJSON), MaxMessageLength)
	msg, err := s.client.SendMessageMarkup(ctx, s.chatID, text, keyboard)
	return msg, false, err
}

// unsent returns the ids of the clips pending review that have no review
// message, oldest first. clips_review serves the query.
func (s *Sender) unsent(ctx context.Context) ([]int64, error) {
	rows, err := s.store.Reader().QueryContext(ctx, `
		SELECT id FROM clips
		WHERE review_status = ? AND telegram_message_id IS NULL
		ORDER BY created_at, id`, store.ReviewPending)
	if err != nil {
		return nil, fmt.Errorf("review: find unsent clips: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("review: find unsent clips: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("review: find unsent clips: %w", err)
	}
	return ids, nil
}

// load reads what the review message of a clip shows and the path of its
// file. ok is false when the clip no longer needs a review message.
func (s *Sender) load(ctx context.Context, id int64) (info clipInfo, file string, ok bool, err error) {
	var (
		status    store.ReviewStatus
		messageID sql.NullInt64
		path      string
		display   string
		login     string
	)
	info.ClipID = id
	err = s.store.Reader().QueryRowContext(ctx, `
		SELECT c.review_status, c.telegram_message_id, c.file_path, c.duration_seconds, c.width, c.height,
			c.transcript_json, cand.start_seconds, v.title, s.display_name, s.login
		FROM clips c
		JOIN jobs j ON j.id = c.job_id
		JOIN candidates cand ON cand.id = j.candidate_id
		JOIN vods v ON v.id = cand.vod_id
		JOIN streamers s ON s.id = v.streamer_id
		WHERE c.id = ?`, id).Scan(
		&status, &messageID, &path, &info.DurationSeconds, &info.width, &info.height,
		&info.TranscriptJSON, &info.OffsetSeconds, &info.VODTitle, &display, &login)
	if errors.Is(err, sql.ErrNoRows) {
		return info, "", false, nil
	}
	if err != nil {
		return info, "", false, fmt.Errorf("review: load clip %d: %w", id, err)
	}
	if status != store.ReviewPending || messageID.Valid {
		return info, "", false, nil
	}
	info.Streamer = display
	if clean(display) == "" {
		info.Streamer = login
	}
	info.PostTitle = s.posttextTitle(ctx, id)
	return info, filepath.Join(s.dataDir, filepath.FromSlash(path)), true, nil
}

// record stores the review message on the clip, in a short write
// transaction of its own. A clip that got a message recorded in the meantime
// keeps that one.
func (s *Sender) record(ctx context.Context, id int64, msg *Message) error {
	chatID := msg.Chat.ID
	if chatID == 0 {
		chatID = s.chatID
	}
	var recorded bool
	err := s.store.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE clips SET telegram_chat_id = ?, telegram_message_id = ?, updated_at = ?
			WHERE id = ? AND telegram_message_id IS NULL`,
			chatID, msg.MessageID, store.FormatTime(s.now()), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		recorded = n == 1
		return err
	})
	if err != nil {
		return fmt.Errorf("review: record message of clip %d: %w", id, err)
	}
	if !recorded {
		s.log.Warn("review: clip already had a review message; the new one is not recorded",
			"clip", id, "message_id", msg.MessageID)
	}
	return nil
}
