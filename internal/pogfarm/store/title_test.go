package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTitleOverrideMigration(t *testing.T) {
	s := openCore(t)
	if got := appliedList(t, s.Reader()); !slices.Contains(got, "3:clip_title_override") {
		t.Errorf("applied migrations = %q, want 3:clip_title_override among them", got)
	}
	var typ string
	var notNull int
	err := s.Reader().QueryRow(`SELECT type, "notnull" FROM pragma_table_info('clips') WHERE name = 'title_override'`).
		Scan(&typ, &notNull)
	if err != nil {
		t.Fatalf("title_override column: %v", err)
	}
	if typ != "TEXT" || notNull != 0 {
		t.Errorf("title_override is %s, notnull %d; want nullable TEXT", typ, notNull)
	}
}

func TestTitleOverride(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	c := f.clip()
	w := f.s.Writer()

	if got, err := GetTitleOverride(ctx, f.s.Reader(), c.ID); err != nil || got != "" {
		t.Fatalf("new clip: override %q, %v; want none", got, err)
	}
	if err := SetTitleOverride(ctx, w, c.ID, "Bärenstark 🐻 moment"); err != nil {
		t.Fatal(err)
	}
	if got, err := GetTitleOverride(ctx, f.s.Reader(), c.ID); err != nil || got != "Bärenstark 🐻 moment" {
		t.Fatalf("override = %q, %v", got, err)
	}
	// Replacing and clearing.
	if err := SetTitleOverride(ctx, w, c.ID, "second"); err != nil {
		t.Fatal(err)
	}
	if got, _ := GetTitleOverride(ctx, f.s.Reader(), c.ID); got != "second" {
		t.Errorf("override = %q, want second", got)
	}
	if err := SetTitleOverride(ctx, w, c.ID, ""); err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := f.s.Reader().QueryRow(`SELECT title_override IS NULL FROM clips WHERE id = ?`, c.ID).Scan(&isNull); err != nil || !isNull {
		t.Errorf("cleared override is not NULL: %v, %v", isNull, err)
	}

	// The CHECK keeps an empty or runaway value out of the column.
	if err := f.exec(`UPDATE clips SET title_override = '' WHERE id = ?`, c.ID); err == nil {
		t.Error("empty title_override accepted")
	}
	long := strings.Repeat("é", MaxTitleOverrideLength)
	if err := SetTitleOverride(ctx, w, c.ID, long); err != nil {
		t.Errorf("override of %d runes refused: %v", MaxTitleOverrideLength, err)
	}
	if err := SetTitleOverride(ctx, w, c.ID, long+"x"); err == nil {
		t.Error("override longer than the limit accepted")
	}

	// Other updates leave it alone.
	if err := SetTitleOverride(ctx, w, c.ID, "kept"); err != nil {
		t.Fatal(err)
	}
	c.TelegramChatID, c.TelegramMessageID = 7, 70
	if err := UpdateClip(ctx, w, c); err != nil {
		t.Fatal(err)
	}
	if got, _ := GetTitleOverride(ctx, f.s.Reader(), c.ID); got != "kept" {
		t.Errorf("override after UpdateClip = %q, want kept", got)
	}
}

func TestTitleOverrideOnlyWhilePending(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	c := f.clip()
	if err := SetTitleOverride(ctx, f.s.Writer(), c.ID, "before"); err != nil {
		t.Fatal(err)
	}
	c.ReviewStatus, c.ReviewedAt = ReviewApproved, time.Now()
	if err := UpdateClip(ctx, f.s.Writer(), c); err != nil {
		t.Fatal(err)
	}
	err := SetTitleOverride(ctx, f.s.Writer(), c.ID, "after")
	if !errors.Is(err, ErrNotPending) {
		t.Errorf("decided clip: error = %v, want ErrNotPending", err)
	}
	if got, _ := GetTitleOverride(ctx, f.s.Reader(), c.ID); got != "before" {
		t.Errorf("override = %q, want the one set before the decision", got)
	}
	wantNotFound(t, "missing clip", SetTitleOverride(ctx, f.s.Writer(), c.ID+100, "x"))
	_, err = GetTitleOverride(ctx, f.s.Reader(), c.ID+100)
	wantNotFound(t, "get of missing clip", err)
}

func TestGetClipByReviewMessage(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	a, b := f.clip(), f.clip()
	a.TelegramChatID, a.TelegramMessageID = 10, 5
	b.TelegramChatID, b.TelegramMessageID = 11, 5
	for _, c := range []Clip{a, b} {
		if err := UpdateClip(ctx, f.s.Writer(), c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := GetClipByReviewMessage(ctx, f.s.Reader(), 11, 5)
	if err != nil || got.ID != b.ID {
		t.Errorf("chat 11 message 5 = clip %d, %v; want %d", got.ID, err, b.ID)
	}
	_, err = GetClipByReviewMessage(ctx, f.s.Reader(), 10, 6)
	wantNotFound(t, "unknown message", err)
}
