package scan

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// Discoverer finds the new VODs of the streamers on the allowlist and records
// them in the store for the scanner. It never looks at a streamer that is not
// on the allowlist or is switched off there.
//
// Discover runs one pass, and Run runs passes at an interval. One Discoverer
// must not run two passes at the same time.
type Discoverer struct {
	// Store is where streamers and VODs are recorded. Required.
	Store *store.Store
	// Sources are the platforms VODs are listed from. Required.
	Sources *source.Registry
	// Allowlist is the streamers to look at, as loaded from the
	// configuration.
	Allowlist []config.Streamer
	// MaxVODAge is how long after its start a VOD is still recorded. Zero
	// means any age.
	MaxVODAge time.Duration
	// Interval is the time between two passes of Run. Zero means
	// DefaultDiscoveryInterval.
	Interval time.Duration
	// Logger receives what Run does. Nil discards it.
	Logger *slog.Logger
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// DiscoverResult says what one pass did.
type DiscoverResult struct {
	// Recorded are the VODs this pass added, with scan status new.
	Recorded []store.VOD
	// Known counts the listed VODs that were already recorded.
	Known int
	// Live counts the VODs left out because their stream has not ended.
	Live int
	// TooOld counts the VODs left out because they are older than
	// MaxVODAge.
	TooOld int
	// Disabled counts the streamers this pass switched off in the store
	// because they are no longer on the allowlist, or are switched off
	// there.
	Disabled int
	// Failures are the streamers whose VODs could not be listed. The pass
	// went on with the others.
	Failures []DiscoverFailure
}

// DiscoverFailure is one allowlist entry a pass could not process.
type DiscoverFailure struct {
	Platform api.Platform
	Login    string
	// Err is why. It wraps source.ErrStreamerNotFound when the platform
	// has no account with that login, and source.ErrUnknownPlatform when
	// no source is registered for the platform.
	Err error
}

func (f DiscoverFailure) Error() string {
	return fmt.Sprintf("discover %s/%s: %v", f.Platform, f.Login, f.Err)
}

func (f DiscoverFailure) Unwrap() error { return f.Err }

// Discover runs one pass over the allowlist.
//
// It first brings the store in line with the allowlist: a stored streamer
// that is not an enabled entry is switched off. Then, for each enabled entry
// in order, it lists the streamer's recent VODs, stores or updates the
// streamer, and records every VOD that has ended, is not older than
// MaxVODAge and is not recorded yet. A VOD is recorded exactly once, however
// often and from however many passes it is seen: the store's uniqueness of
// (platform, platform VOD id) decides, not a lookup beforehand.
//
// A streamer whose VODs cannot be listed is reported in the result and does
// not stop the pass. The returned error is the context's when ctx ends, and
// otherwise a failure of the store; the result then says what was done up to
// that point.
//
// A streamer is stored the first time the platform lists a VOD for them, live
// or old ones included, unless SyncStreamers stored them before.
func (d *Discoverer) Discover(ctx context.Context) (DiscoverResult, error) {
	var res DiscoverResult
	if d.Store == nil || d.Sources == nil {
		return res, errors.New("scan: discoverer needs a store and sources")
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	disabled, err := d.syncAllowlist(ctx)
	res.Disabled = disabled
	if err != nil {
		return res, fmt.Errorf("scan: sync allowlist: %w", err)
	}

	for _, entry := range d.Allowlist {
		if !entry.On() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		err := d.discoverStreamer(ctx, entry, &res)
		if err == nil {
			continue
		}
		var storeErr *discoverStoreError
		switch {
		case ctx.Err() != nil:
			return res, ctx.Err()
		case errors.As(err, &storeErr):
			return res, fmt.Errorf("scan: discover %s/%s: %w", entry.Platform, entry.Login, storeErr.err)
		}
		res.Failures = append(res.Failures, DiscoverFailure{Platform: entry.Platform, Login: entry.Login, Err: err})
	}
	return res, nil
}

// discoverStoreError marks a failure of the store, which ends the pass.
type discoverStoreError struct{ err error }

func (e *discoverStoreError) Error() string { return e.err.Error() }

// discoverStreamer handles one enabled allowlist entry. An error that is not
// a *discoverStoreError concerns this streamer only.
func (d *Discoverer) discoverStreamer(ctx context.Context, entry config.Streamer, res *DiscoverResult) error {
	src, err := d.Sources.Get(entry.Platform)
	if err != nil {
		return err
	}
	vods, err := src.RecentVODs(ctx, entry.Login)
	if err != nil {
		return err
	}
	if len(vods) == 0 {
		return nil
	}
	for _, v := range vods {
		if err := checkDiscovered(v, entry.Platform, vods[0].StreamerID); err != nil {
			return err
		}
	}

	streamer, err := d.upsertStreamer(ctx, entry, vods[0].StreamerID, vods[0].Streamer.DisplayName)
	if err != nil {
		return &discoverStoreError{err}
	}

	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	// Oldest first, so VODs get row ids in the order they were streamed.
	for _, v := range slices.Backward(vods) {
		switch {
		case !v.Ended:
			res.Live++
			continue
		case d.MaxVODAge > 0 && now.Sub(v.StartedAt) > d.MaxVODAge:
			res.TooOld++
			continue
		}
		row, err := store.InsertVOD(ctx, d.Store.Writer(), store.VOD{
			Platform:        entry.Platform,
			PlatformVODID:   v.ID,
			StreamerID:      streamer.ID,
			Title:           v.Title,
			DurationSeconds: v.Duration.Seconds(),
			StartedAt:       v.StartedAt,
			EndedAt:         v.StartedAt.Add(v.Duration),
			ScanStatus:      store.ScanNew,
		})
		switch {
		case errors.Is(err, store.ErrDuplicate):
			res.Known++
		case err != nil:
			return &discoverStoreError{err}
		default:
			res.Recorded = append(res.Recorded, row)
		}
	}
	return nil
}

// checkDiscovered rejects a VOD the store could not hold, or that does not
// belong to the listing it came in.
func checkDiscovered(v source.VOD, platform api.Platform, streamerID string) error {
	switch {
	case v.ID == "":
		return errors.New("source listed a vod without an id")
	case v.Platform != platform:
		return fmt.Errorf("source listed vod %q with platform %q", v.ID, v.Platform)
	case v.StreamerID == "":
		return fmt.Errorf("source listed vod %q without the streamer's id", v.ID)
	case v.StreamerID != streamerID:
		return fmt.Errorf("source listed vod %q of another streamer", v.ID)
	case v.StartedAt.IsZero():
		return fmt.Errorf("source listed vod %q without a start time", v.ID)
	case v.Duration < 0:
		return fmt.Errorf("source listed vod %q with a negative duration", v.ID)
	}
	return nil
}

// upsertStreamer stores the streamer with the given platform user id as
// enabled, or brings the stored row up to date. The insert goes first and the store's
// uniqueness of (platform, platform user id) tells whether the row existed.
// The stored login is the allowlist's, which is what syncAllowlist compares
// with; a streamer who was renamed keeps their row and gets the new login.
func (d *Discoverer) upsertStreamer(ctx context.Context, entry config.Streamer, platformUserID, displayName string) (store.Streamer, error) {
	want := store.Streamer{
		Platform:       entry.Platform,
		PlatformUserID: platformUserID,
		Login:          entry.Login,
		DisplayName:    displayName,
		Enabled:        true,
	}
	row, err := store.InsertStreamer(ctx, d.Store.Writer(), want)
	if !errors.Is(err, store.ErrDuplicate) {
		return row, err
	}
	row, err = store.GetStreamerByPlatformID(ctx, d.Store.Writer(), entry.Platform, platformUserID)
	if err != nil {
		return row, err
	}
	if row.Login == want.Login && row.DisplayName == want.DisplayName && row.Enabled {
		return row, nil
	}
	row.Login, row.DisplayName, row.Enabled = want.Login, want.DisplayName, true
	return row, store.UpdateStreamer(ctx, d.Store.Writer(), row)
}

// syncAllowlist switches off every stored streamer that is not an enabled
// allowlist entry, and returns how many it switched off. Enabled entries are
// stored, and switched back on, by SyncStreamers and when their VODs are
// listed.
func (d *Discoverer) syncAllowlist(ctx context.Context) (int, error) {
	rows, err := d.Store.Reader().QueryContext(ctx, `SELECT id, platform, login FROM streamers WHERE enabled = 1`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var off []int64
	for rows.Next() {
		var (
			id       int64
			platform api.Platform
			login    string
		)
		if err := rows.Scan(&id, &platform, &login); err != nil {
			return 0, err
		}
		allowed := slices.ContainsFunc(d.Allowlist, func(e config.Streamer) bool {
			return e.On() && e.Platform == platform && strings.EqualFold(e.Login, login)
		})
		if !allowed {
			off = append(off, id)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	disabled := 0
	for _, id := range off {
		_, err := d.Store.Writer().ExecContext(ctx,
			`UPDATE streamers SET enabled = 0, updated_at = ? WHERE id = ?`, store.FormatTime(time.Now()), id)
		if err != nil {
			return disabled, err
		}
		disabled++
	}
	return disabled, nil
}
