package twitch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
)

const (
	// clipsPageSize is how many clips one request asks for. It is the
	// largest page Helix serves.
	clipsPageSize = 100
	// clipsMaxPages bounds how many pages one listing follows, so a
	// cursor that never ends cannot keep a caller busy forever. Helix
	// lists the most viewed clips first, so what is cut off is the least
	// viewed tail.
	clipsMaxPages = 50
	// clipsCreatedGrace is how long after the end of a VOD a clip of it may
	// have been created and still be listed. Helix filters clips by when
	// they were created, and viewers also cut clips out of the VOD after
	// the stream has ended.
	clipsCreatedGrace = 24 * time.Hour
)

// ClipLister lists the clips viewers made of a Twitch VOD. Its ViewerClips
// method is the one source.ViewerClipLister asks for. It is safe for
// concurrent use.
type ClipLister struct {
	client *Client
}

var _ source.ViewerClipLister = (*ClipLister)(nil)

// NewClipLister returns a lister that makes its requests through c.
func NewClipLister(c *Client) *ClipLister {
	return &ClipLister{client: c}
}

// clipsVideo is the part of a Helix video ViewerClips needs.
type clipsVideo struct {
	UserID string `json:"user_id"`
}

// clipsItem is one clip as Helix returns it.
type clipsItem struct {
	ID string `json:"id"`
	// VideoID is the id of the VOD the clip was cut from. It is empty when
	// the VOD is not available, or not yet linked to the clip.
	VideoID string `json:"video_id"`
	Title   string `json:"title"`
	Views   int    `json:"view_count"`
	// Duration is the length of the clip in seconds, with a fraction.
	Duration float64 `json:"duration"`
	// VODOffset is where the clip starts in the VOD, in seconds. It is null
	// when VideoID is empty.
	VODOffset *float64 `json:"vod_offset"`
}

// clipsPage is one page of the Helix clips endpoint.
type clipsPage struct {
	Data       []clipsItem `json:"data"`
	Pagination struct {
		Cursor string `json:"cursor"`
	} `json:"pagination"`
}

// ViewerClips lists the viewer-made clips of a VOD, ordered by offset. Clips
// that start at the same offset are ordered most viewed first.
//
// It asks Helix for the clips of the VOD's broadcaster that were created
// between the start of the VOD and a day after its end, follows the
// pagination, and keeps the clips Helix links to this VOD. A clip that has
// no offset into the VOD, or belongs to another VOD, is dropped. At most
// 5000 clips of the broadcaster are examined.
//
// vod.ID and vod.StartedAt must be set. The error wraps
// source.ErrVODNotFound when Twitch no longer has the VOD.
func (l *ClipLister) ViewerClips(ctx context.Context, vod source.VOD) ([]source.ViewerClip, error) {
	clips, err := l.viewerClips(ctx, vod)
	if err != nil {
		return nil, fmt.Errorf("twitch: viewer clips of vod %q: %w", vod.ID, err)
	}
	return clips, nil
}

func (l *ClipLister) viewerClips(ctx context.Context, vod source.VOD) ([]source.ViewerClip, error) {
	if vod.ID == "" {
		return nil, errors.New("vod has no id")
	}
	if vod.StartedAt.IsZero() {
		return nil, errors.New("vod has no start time")
	}
	broadcaster, err := l.clipsBroadcaster(ctx, vod.ID)
	if err != nil {
		return nil, err
	}

	query := url.Values{
		"broadcaster_id": {broadcaster},
		"started_at":     {vod.StartedAt.UTC().Format(time.RFC3339)},
		"ended_at":       {vod.StartedAt.Add(vod.Duration + clipsCreatedGrace).UTC().Format(time.RFC3339)},
		"first":          {strconv.Itoa(clipsPageSize)},
	}
	type found struct {
		clip source.ViewerClip
		seq  int // position in the listing, to keep the sort stable
	}
	var kept []found
	seen := make(map[string]bool) // clip ids, since pages can overlap
	for page := 0; page < clipsMaxPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var resp clipsPage
		if err := l.client.Get(ctx, "clips", query, &resp); err != nil {
			return nil, err
		}
		for _, c := range resp.Data {
			if c.VideoID != vod.ID || c.VODOffset == nil || *c.VODOffset < 0 {
				continue
			}
			if c.ID != "" {
				if seen[c.ID] {
					continue
				}
				seen[c.ID] = true
			}
			kept = append(kept, found{
				clip: source.ViewerClip{
					Offset:   clipsSeconds(*c.VODOffset),
					Duration: clipsSeconds(c.Duration),
					Views:    c.Views,
					Title:    c.Title,
				},
				seq: len(kept),
			})
		}
		if resp.Pagination.Cursor == "" {
			break
		}
		query.Set("after", resp.Pagination.Cursor)
	}

	slices.SortFunc(kept, func(a, b found) int {
		return cmp.Or(
			cmp.Compare(a.clip.Offset, b.clip.Offset),
			cmp.Compare(b.clip.Views, a.clip.Views),
			cmp.Compare(a.seq, b.seq),
		)
	})
	clips := make([]source.ViewerClip, len(kept))
	for i, f := range kept {
		clips[i] = f.clip
	}
	return clips, nil
}

// clipsBroadcaster returns the user id of the broadcaster a VOD belongs to,
// which the clips endpoint is queried by. The error wraps
// source.ErrVODNotFound when Twitch does not have the VOD.
func (l *ClipLister) clipsBroadcaster(ctx context.Context, vodID string) (string, error) {
	var resp struct {
		Data []clipsVideo `json:"data"`
	}
	err := l.client.Get(ctx, "videos", url.Values{"id": {vodID}}, &resp)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return "", fmt.Errorf("%w: %w", source.ErrVODNotFound, err)
	}
	if err != nil {
		return "", err
	}
	if len(resp.Data) == 0 {
		return "", source.ErrVODNotFound
	}
	if resp.Data[0].UserID == "" {
		return "", errors.New("twitch did not name the broadcaster of the vod")
	}
	return resp.Data[0].UserID, nil
}

// clipsSeconds converts a number of seconds to a duration, rounded to the
// millisecond so a fraction such as 29.9 does not carry float noise.
func clipsSeconds(s float64) time.Duration {
	return time.Duration(math.Round(s*1000)) * time.Millisecond
}
