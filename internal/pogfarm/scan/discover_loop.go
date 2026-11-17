package scan

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// DefaultDiscoveryInterval is the time between two discovery passes when
// Discoverer.Interval is zero.
const DefaultDiscoveryInterval = 15 * time.Minute

// SyncResult says what SyncStreamers did.
type SyncResult struct {
	// Stored are the streamer rows of the enabled allowlist entries, as
	// created or brought up to date.
	Stored []store.Streamer
	// Lazy counts the enabled entries whose platform cannot look up an
	// account without listing VODs. Their row is created when the first
	// VOD is listed, as before.
	Lazy int
	// Disabled counts the stored streamers switched off because they are
	// no longer enabled on the allowlist.
	Disabled int
	// Failures are the entries whose account could not be looked up. The
	// sync went on with the others.
	Failures []DiscoverFailure
}

// SyncStreamers brings the streamers table in line with the allowlist
// without listing any VODs: a stored streamer that is not an enabled entry
// is switched off, and each enabled entry is looked up on its platform
// (source.LookupStreamer) and stored as enabled, or brought up to date, under
// the account's permanent id. Run calls it once at startup, so a streamer
// has a row as soon as they are on the allowlist, not only once they have a
// VOD.
//
// An entry whose account cannot be looked up is reported in the result and
// does not stop the sync. The returned error is the context's when ctx ends,
// and otherwise a failure of the store.
func (d *Discoverer) SyncStreamers(ctx context.Context) (SyncResult, error) {
	var res SyncResult
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
		fail := func(err error) {
			res.Failures = append(res.Failures, DiscoverFailure{Platform: entry.Platform, Login: entry.Login, Err: err})
		}
		src, err := d.Sources.Get(entry.Platform)
		if err != nil {
			fail(err)
			continue
		}
		acct, err := source.LookupStreamer(ctx, src, entry.Login)
		switch {
		case errors.Is(err, source.ErrLookupUnsupported):
			res.Lazy++
			continue
		case ctx.Err() != nil:
			return res, ctx.Err()
		case err != nil:
			fail(err)
			continue
		case acct.ID == "":
			fail(errors.New("source returned an account without an id"))
			continue
		}
		row, err := d.upsertStreamer(ctx, entry, acct.ID, acct.Streamer.DisplayName)
		if err != nil {
			return res, fmt.Errorf("scan: sync streamer %s/%s: %w", entry.Platform, entry.Login, err)
		}
		res.Stored = append(res.Stored, row)
	}
	return res, nil
}

// Run syncs the allowlist into the store with SyncStreamers, then runs a
// discovery pass right away and one every Interval until ctx ends. Every
// failure is logged and none stops the loop: a failed pass is followed by
// the next one on schedule.
//
// Run returns an error only when the discoverer is misconfigured, before any
// pass. Otherwise it returns nil once ctx has ended and the pass in progress
// has stopped.
func (d *Discoverer) Run(ctx context.Context) error {
	if d.Store == nil || d.Sources == nil {
		return errors.New("scan: discoverer needs a store and sources")
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	interval := d.Interval
	if interval <= 0 {
		interval = DefaultDiscoveryInterval
	}

	synced, err := d.SyncStreamers(ctx)
	switch {
	case ctx.Err() != nil:
		return nil
	case err != nil:
		log.Error("allowlist sync failed", "error", err)
	default:
		log.Info("allowlist synced", "stored", len(synced.Stored), "lazy", synced.Lazy,
			"disabled", synced.Disabled, "failed", len(synced.Failures))
	}
	for _, f := range synced.Failures {
		log.Warn("allowlist sync: streamer not stored", "platform", f.Platform, "login", f.Login, "error", f.Err)
	}

	for {
		res, err := d.Discover(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			log.Error("discovery pass failed", "error", err)
		} else {
			log.Info("discovery pass done", "recorded", len(res.Recorded), "known", res.Known,
				"live", res.Live, "too_old", res.TooOld, "disabled", res.Disabled, "failed", len(res.Failures))
		}
		for _, f := range res.Failures {
			log.Warn("discovery: streamer skipped", "platform", f.Platform, "login", f.Login, "error", f.Err)
		}
		if !sleep(ctx, interval) {
			return nil
		}
	}
}
