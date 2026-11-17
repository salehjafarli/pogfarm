package review

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// ActionUndo is the callback action of the Undo button shown under a review
// message for a while after a decision: "undo:42".
const ActionUndo = "undo"

// DefaultUndoWindow is how long a decision can be undone when
// DeciderOptions.UndoWindow is zero. It matches the config default.
const DefaultUndoWindow = 5 * time.Minute

// decideMarker starts the line a decided review message begins with. Undo
// removes that line again by looking for it.
const decideMarker = "Decision: "

// DeciderOptions configure a Decider.
type DeciderOptions struct {
	// Store holds the clips and their uploads. Required.
	Store *store.Store
	// Client answers the button taps and edits the review messages.
	// Required.
	Client *Client
	// Targets are the names of the enabled upload targets, such as
	// "youtube". Approving a clip creates one pending upload row for each.
	// With none, an approved clip gets no upload rows.
	Targets []string
	// UndoWindow is how long after a decision it can be undone. Zero means
	// DefaultUndoWindow.
	UndoWindow time.Duration
	// Now is the clock. Default time.Now.
	Now func() time.Time
	// Logger receives the decisions and failed Telegram calls. Default:
	// discarded.
	Logger *slog.Logger
}

// Decider handles the Approve, Reject and Undo buttons of the review
// messages. It is safe for concurrent use.
//
// A decision is one write transaction: a conditional update of the clip from
// pending to approved or rejected, with reviewed_at set, and for an approval
// the insert of one pending upload row per enabled target. A tap on a clip
// that is already decided changes nothing and is answered with the existing
// decision, so repeated, late and crossed taps are harmless. Rejecting is all
// cleanup needs: it deletes the file of a rejected clip once the retention
// period, counted from reviewed_at, has passed.
//
// Undo is allowed within UndoWindow of reviewed_at, and only while none of
// the clip's upload rows has been started (every row is pending and was
// never attempted). It deletes the pending upload rows and returns the clip
// to pending, in one write transaction. The uploader marks a row uploading in
// a write transaction of its own before it calls a platform, and the store
// runs write transactions one at a time, so an undo and the start of an
// upload can never both happen.
//
// Telegram calls are made after the transaction has committed. The message
// tapped is edited to show the state of the clip: a decided message starts
// with a "Decision:" line and carries an Undo button while undo is possible.
// The Undo button is not removed the moment the window closes, which would
// need a timer per message; a tap on it after that is refused and removes
// it.
type Decider struct {
	store   *store.Store
	client  *Client
	targets []string
	window  time.Duration
	now     func() time.Time
	log     *slog.Logger
}

// NewDecider returns a decider. Target names must be non-empty and distinct.
func NewDecider(opts DeciderOptions) (*Decider, error) {
	if opts.Store == nil {
		return nil, errors.New("review: decider needs a store")
	}
	if opts.Client == nil {
		return nil, errors.New("review: decider needs a Telegram client")
	}
	if opts.UndoWindow < 0 {
		return nil, fmt.Errorf("review: undo window %s is negative", opts.UndoWindow)
	}
	for i, t := range opts.Targets {
		if strings.TrimSpace(t) == "" {
			return nil, errors.New("review: an upload target name is empty")
		}
		if slices.Contains(opts.Targets[:i], t) {
			return nil, fmt.Errorf("review: upload target %q is listed twice", t)
		}
	}
	d := &Decider{
		store:   opts.Store,
		client:  opts.Client,
		targets: slices.Clone(opts.Targets),
		window:  opts.UndoWindow,
		now:     opts.Now,
		log:     opts.Logger,
	}
	if d.window == 0 {
		d.window = DefaultUndoWindow
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	return d, nil
}

// Register makes b route the Approve, Reject and Undo buttons to d.
func (d *Decider) Register(b *Bot) {
	b.HandleCallback(ActionApprove, d.handleApprove)
	b.HandleCallback(ActionReject, d.handleReject)
	b.HandleCallback(ActionUndo, d.handleUndo)
}

func (d *Decider) handleApprove(ctx context.Context, q *CallbackQuery, payload string) error {
	return d.handleDecision(ctx, q, payload, store.ReviewApproved)
}

func (d *Decider) handleReject(ctx context.Context, q *CallbackQuery, payload string) error {
	return d.handleDecision(ctx, q, payload, store.ReviewRejected)
}

// decideState is a clip as a button tap leaves it.
type decideState struct {
	found bool
	clip  store.Clip
	// started is whether an upload of the clip has started.
	started bool
}

// undoable reports whether the decision on the clip can still be undone at
// now.
func (d *Decider) undoable(s decideState, now time.Time) bool {
	return s.found && s.clip.ReviewStatus != store.ReviewPending && !s.started &&
		now.Sub(s.clip.ReviewedAt) < d.window
}

func (d *Decider) handleDecision(ctx context.Context, q *CallbackQuery, payload string, to store.ReviewStatus) error {
	id, err := ParseClipID(payload)
	if err != nil {
		return d.answer(ctx, q, "This button is not valid.")
	}
	now := d.now()
	var changed bool
	var s decideState
	err = d.store.WriteTx(ctx, func(tx *sql.Tx) error {
		changed, s = false, decideState{}
		res, err := tx.ExecContext(ctx, `
			UPDATE clips SET review_status = ?, reviewed_at = ?, updated_at = ?
			WHERE id = ? AND review_status = ?`,
			to, store.FormatTime(now), store.FormatTime(now), id, store.ReviewPending)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		changed = n == 1
		if changed && to == store.ReviewApproved {
			for _, target := range d.targets {
				_, err := store.InsertUpload(ctx, tx, store.Upload{ClipID: id, TargetPlatform: target, CreatedAt: now})
				// A row for the target already there is a row the uploader
				// already has: keep it.
				if err != nil && !errors.Is(err, store.ErrDuplicate) {
					return err
				}
			}
		}
		s, err = decideLoad(ctx, tx, id)
		return err
	})
	if err != nil {
		answerErr := d.answer(ctx, q, "The decision could not be saved. Try again.")
		return errors.Join(fmt.Errorf("review: decide clip %d: %w", id, err), answerErr)
	}
	if !s.found {
		return d.answer(ctx, q, "This clip no longer exists.")
	}

	var notice string
	switch {
	case changed:
		d.log.Info("review: clip decided", "clip", id, "decision", to, "targets", len(d.targets))
		notice = decidedWord(to) + "."
	case s.clip.ReviewStatus == to:
		notice = "Already " + strings.ToLower(decidedWord(to)) + "."
	default:
		notice = "Already " + strings.ToLower(decidedWord(s.clip.ReviewStatus)) + "."
		if d.undoable(s, now) {
			notice += " Tap Undo to change it."
		}
	}
	return errors.Join(d.answer(ctx, q, notice), d.show(ctx, q, s, d.undoable(s, now)))
}

// Outcomes of an undo.
type undoOutcome int

const (
	undoDone undoOutcome = iota
	undoAlreadyPending
	undoExpired
	undoStarted
)

func (d *Decider) handleUndo(ctx context.Context, q *CallbackQuery, payload string) error {
	id, err := ParseClipID(payload)
	if err != nil {
		return d.answer(ctx, q, "This button is not valid.")
	}
	now := d.now()
	var outcome undoOutcome
	var s decideState
	err = d.store.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		s, err = decideLoad(ctx, tx, id)
		switch {
		case err != nil || !s.found:
			return err
		case s.clip.ReviewStatus == store.ReviewPending:
			outcome = undoAlreadyPending
			return nil
		case s.started:
			outcome = undoStarted
			return nil
		case now.Sub(s.clip.ReviewedAt) >= d.window:
			outcome = undoExpired
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM uploads WHERE clip_id = ? AND status = ?`,
			id, store.UploadPending); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE clips SET review_status = ?, reviewed_at = NULL, updated_at = ?
			WHERE id = ? AND review_status = ?`,
			store.ReviewPending, store.FormatTime(now), id, s.clip.ReviewStatus)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			// Impossible inside this transaction; rolled back if it happens.
			return errors.Join(err, fmt.Errorf("clip %d changed during undo", id))
		}
		outcome = undoDone
		s, err = decideLoad(ctx, tx, id)
		return err
	})
	if err != nil {
		answerErr := d.answer(ctx, q, "Undo could not be saved. Try again.")
		return errors.Join(fmt.Errorf("review: undo clip %d: %w", id, err), answerErr)
	}
	if !s.found {
		return d.answer(ctx, q, "This clip no longer exists.")
	}

	var notice string
	switch outcome {
	case undoDone:
		d.log.Info("review: decision undone", "clip", id)
		notice = "Undone: the clip is back in review."
	case undoAlreadyPending:
		notice = "The clip is already back in review."
	case undoStarted:
		notice = "Undo is no longer possible: posting has started."
	case undoExpired:
		notice = "Undo is no longer possible: the undo window has passed."
	}
	return errors.Join(d.answer(ctx, q, notice), d.show(ctx, q, s, false))
}

// decideLoad reads a clip and whether any of its uploads has started: a row
// that is not pending, or a pending row that was already attempted.
func decideLoad(ctx context.Context, q store.Querier, id int64) (decideState, error) {
	clip, err := store.GetClip(ctx, q, id)
	if errors.Is(err, store.ErrNotFound) {
		return decideState{}, nil
	}
	if err != nil {
		return decideState{}, err
	}
	var started bool
	err = q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM uploads WHERE clip_id = ? AND (status <> ? OR attempts > 0))`,
		id, store.UploadPending).Scan(&started)
	if err != nil {
		return decideState{}, fmt.Errorf("store: uploads of clip %d: %w", id, err)
	}
	return decideState{found: true, clip: clip, started: started}, nil
}

func (d *Decider) answer(ctx context.Context, q *CallbackQuery, text string) error {
	return d.client.AnswerCallbackQuery(ctx, q.ID, text)
}

// show edits the tapped review message to show the state of the clip: the
// review buttons for a pending clip; for a decided one a decision line at
// the top and an Undo button when undo is true.
//
// A text message is recognised by its text; any other message the bot sends
// for review is a video, whose caption is rebuilt from the database. When
// the tap carries no usable message (Telegram leaves it out for very old
// ones), the message recorded on the clip gets its buttons replaced and its
// content is left alone.
func (d *Decider) show(ctx context.Context, q *CallbackQuery, s decideState, undo bool) error {
	clip := s.clip
	var markup *InlineKeyboardMarkup
	switch {
	case clip.ReviewStatus == store.ReviewPending:
		markup = ReviewKeyboard(clip.ID)
	case undo:
		markup = &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{
			{Text: "Undo (" + formatWindow(d.window) + ")", CallbackData: CallbackData(ActionUndo, clip.ID)},
		}}}
	}
	line := ""
	if clip.ReviewStatus != store.ReviewPending {
		line = decisionLine(clip.ReviewStatus)
	}

	m := q.Message
	var err error
	switch {
	case m == nil || m.Date == 0:
		// No message, or one Telegram no longer shows the bot.
		if clip.TelegramMessageID == 0 {
			return nil
		}
		err = d.client.EditMessageReplyMarkup(ctx, clip.TelegramChatID, clip.TelegramMessageID, markup)
	case m.Text != "":
		text := decideStrip(m.Text)
		if line != "" {
			text = truncateText(line+"\n\n"+text, MaxMessageLength)
		}
		err = d.client.EditMessageText(ctx, m.Chat.ID, m.MessageID, text, markup)
	default:
		info, ok, loadErr := decideInfo(ctx, d.store, clip.ID)
		if loadErr != nil || !ok {
			d.log.Warn("review: cannot rebuild the caption, changing the buttons only", "clip", clip.ID, "err", loadErr)
			err = d.client.EditMessageReplyMarkup(ctx, m.Chat.ID, m.MessageID, markup)
			break
		}
		caption := info.caption(MaxCaptionLength)
		if line != "" {
			caption = truncateText(line+"\n\n"+caption, MaxCaptionLength)
		}
		err = d.client.EditMessageCaption(ctx, m.Chat.ID, m.MessageID, caption, markup)
	}
	if err != nil && !IsMessageNotModified(err) {
		return fmt.Errorf("review: update the message of clip %d: %w", clip.ID, err)
	}
	return nil
}

func decidedWord(s store.ReviewStatus) string {
	switch s {
	case store.ReviewApproved:
		return "Approved"
	case store.ReviewRejected:
		return "Rejected"
	}
	return "Pending"
}

func decisionLine(s store.ReviewStatus) string {
	switch s {
	case store.ReviewApproved:
		return decideMarker + "approved, queued for posting."
	case store.ReviewRejected:
		return decideMarker + "rejected."
	}
	return ""
}

// decideStrip removes the decision line a decided text message starts with.
func decideStrip(text string) string {
	if !strings.HasPrefix(text, decideMarker) {
		return text
	}
	if _, rest, ok := strings.Cut(text, "\n\n"); ok {
		return rest
	}
	return text
}

// formatWindow writes the undo window for a button: "5 min", or the
// duration as Go writes it when it is not whole minutes.
func formatWindow(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%d min", d/time.Minute)
	}
	return d.String()
}

// decideInfo reads what the caption of a clip's review message shows. ok is
// false when the clip does not exist.
func decideInfo(ctx context.Context, st *store.Store, id int64) (info clipInfo, ok bool, err error) {
	var display, login string
	info.ClipID = id
	err = st.Reader().QueryRowContext(ctx, `
		SELECT c.duration_seconds, c.width, c.height, c.transcript_json, cand.start_seconds, v.title,
			s.display_name, s.login
		FROM clips c
		JOIN jobs j ON j.id = c.job_id
		JOIN candidates cand ON cand.id = j.candidate_id
		JOIN vods v ON v.id = cand.vod_id
		JOIN streamers s ON s.id = v.streamer_id
		WHERE c.id = ?`, id).Scan(
		&info.DurationSeconds, &info.width, &info.height, &info.TranscriptJSON, &info.OffsetSeconds,
		&info.VODTitle, &display, &login)
	if errors.Is(err, sql.ErrNoRows) {
		return info, false, nil
	}
	if err != nil {
		return info, false, err
	}
	info.Streamer = display
	if clean(display) == "" {
		info.Streamer = login
	}
	return info, true, nil
}
