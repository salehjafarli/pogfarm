package twitch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
)

// vodsPageSize is how many videos one listing asks for. It is the largest
// page Helix serves, and RecentVODs reads only that first page: Helix lists
// the newest videos first, so what is cut off is the oldest.
const vodsPageSize = 100

// VODLister lists the recent VODs of a Twitch streamer. Its RecentVODs
// method is the one source.Source asks for. It is safe for concurrent use.
type VODLister struct {
	client *Client
}

// NewVODLister returns a lister that makes its requests through c.
func NewVODLister(c *Client) *VODLister {
	return &VODLister{client: c}
}

// vodsUser is the part of a Helix user RecentVODs needs.
type vodsUser struct {
	ID          string `json:"id"`
	Login       string `json:"login"`
	DisplayName string `json:"display_name"`
}

// vodsVideo is the part of a Helix video RecentVODs needs.
type vodsVideo struct {
	ID string `json:"id"`
	// StreamID is the id of the stream an archive video was recorded from.
	// It is null for other types of video.
	StreamID string `json:"stream_id"`
	Title    string `json:"title"`
	// CreatedAt is when the video was created, in RFC 3339. For an archive
	// that is when the stream started.
	CreatedAt string `json:"created_at"`
	// Duration is the length so far, such as "3h2m1s".
	Duration string `json:"duration"`
	Type     string `json:"type"`
}

// vodsStream is the part of a Helix stream RecentVODs needs. Helix lists a
// stream only while it is live.
type vodsStream struct {
	ID        string `json:"id"`
	StartedAt string `json:"started_at"`
}

// vodsLive is one stream that was live during a listing.
type vodsLive struct {
	id        string
	startedAt time.Time
}

// RecentVODs lists the streamer's archive videos, the recordings of past
// streams, newest first. At most the 100 newest are returned. Highlights and
// uploads are not VODs and are left out.
//
// The VOD of a stream that is still live is included with Ended false. A
// video counts as still live when Helix names the live stream as the one it
// was recorded from, or when the video reaches into the time the live stream
// has been running. Whether the streamer is live is asked both before and
// after the videos are listed, so a stream that starts or ends in between is
// still seen as live; the VOD is then reported as ended on a later call.
//
// The error wraps source.ErrStreamerNotFound when Twitch has no account with
// that login.
func (l *VODLister) RecentVODs(ctx context.Context, login string) ([]source.VOD, error) {
	vods, err := l.recentVODs(ctx, login)
	if err != nil {
		return nil, fmt.Errorf("twitch: recent vods of %q: %w", login, err)
	}
	return vods, nil
}

func (l *VODLister) recentVODs(ctx context.Context, login string) ([]source.VOD, error) {
	if login == "" {
		return nil, errors.New("login is empty")
	}
	user, err := l.vodsUser(ctx, login)
	if err != nil {
		return nil, err
	}

	before, err := l.vodsLiveStreams(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	var videos struct {
		Data []vodsVideo `json:"data"`
	}
	err = l.client.Get(ctx, "videos", url.Values{
		"user_id": {user.ID},
		"type":    {"archive"},
		"first":   {strconv.Itoa(vodsPageSize)},
	}, &videos)
	if err != nil {
		return nil, err
	}
	after, err := l.vodsLiveStreams(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	live := append(before, after...)

	vods := make([]source.VOD, 0, len(videos.Data))
	for _, v := range videos.Data {
		if v.Type != "archive" {
			continue
		}
		if v.ID == "" {
			return nil, errors.New("a video has no id")
		}
		startedAt, err := time.Parse(time.RFC3339, v.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("video %q: creation time %q is not RFC 3339", v.ID, v.CreatedAt)
		}
		duration, err := vodsParseDuration(v.Duration)
		if err != nil {
			return nil, fmt.Errorf("video %q: %w", v.ID, err)
		}
		vods = append(vods, source.VOD{
			Platform:   api.PlatformTwitch,
			ID:         v.ID,
			Streamer:   api.Streamer{Login: user.Login, DisplayName: user.DisplayName},
			StreamerID: user.ID,
			Title:      v.Title,
			StartedAt:  startedAt.UTC(),
			Duration:   duration,
			Ended:      !vodsStillLive(v.StreamID, startedAt.Add(duration), live),
		})
	}
	slices.SortStableFunc(vods, func(a, b source.VOD) int {
		return b.StartedAt.Compare(a.StartedAt)
	})
	return vods, nil
}

// LookupStreamer resolves a login to the Twitch account, whether or not it has
// any VODs. It makes one request. The error wraps source.ErrStreamerNotFound
// when Twitch has no account with that login.
func (l *VODLister) LookupStreamer(ctx context.Context, login string) (source.Account, error) {
	if login == "" {
		return source.Account{}, errors.New("twitch: look up streamer: login is empty")
	}
	user, err := l.vodsUser(ctx, login)
	if err != nil {
		return source.Account{}, fmt.Errorf("twitch: look up streamer %q: %w", login, err)
	}
	return source.Account{
		ID:       user.ID,
		Streamer: api.Streamer{Login: user.Login, DisplayName: user.DisplayName},
	}, nil
}

// vodsUser resolves a login to the account.
func (l *VODLister) vodsUser(ctx context.Context, login string) (vodsUser, error) {
	var users struct {
		Data []vodsUser `json:"data"`
	}
	if err := l.client.Get(ctx, "users", url.Values{"login": {login}}, &users); err != nil {
		return vodsUser{}, err
	}
	// Helix answers a login it does not know with an empty list.
	if len(users.Data) == 0 {
		return vodsUser{}, source.ErrStreamerNotFound
	}
	user := users.Data[0]
	if user.ID == "" {
		return vodsUser{}, errors.New("the user has no id")
	}
	if user.Login == "" {
		user.Login = login
	}
	return user, nil
}

// vodsLiveStreams returns the streams of the user that are live now: none or
// one.
func (l *VODLister) vodsLiveStreams(ctx context.Context, userID string) ([]vodsLive, error) {
	var streams struct {
		Data []vodsStream `json:"data"`
	}
	err := l.client.Get(ctx, "streams", url.Values{"user_id": {userID}, "type": {"live"}}, &streams)
	if err != nil {
		return nil, err
	}
	live := make([]vodsLive, 0, len(streams.Data))
	for _, s := range streams.Data {
		startedAt, err := time.Parse(time.RFC3339, s.StartedAt)
		if err != nil {
			return nil, fmt.Errorf("live stream %q: start time %q is not RFC 3339", s.ID, s.StartedAt)
		}
		live = append(live, vodsLive{id: s.ID, startedAt: startedAt})
	}
	return live, nil
}

// vodsStillLive reports whether a video is the recording of one of the live
// streams: Helix names that stream as the video's origin, or the video, which
// has run until end so far, reaches into the time the stream has been
// running. The second rule covers a video whose stream id is missing; a VOD
// of an earlier stream ended before the live one started.
func vodsStillLive(streamID string, end time.Time, live []vodsLive) bool {
	for _, s := range live {
		if streamID != "" && streamID == s.id {
			return true
		}
		if !end.Before(s.startedAt) {
			return true
		}
	}
	return false
}

// vodsDurationPattern is the length of a video as Helix writes it: hours,
// minutes and seconds, each left out when the length is too short to need
// it, as in "3h2m1s", "21m5s" and "45s".
var vodsDurationPattern = regexp.MustCompile(`^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)

// vodsParseDuration reads the length of a video.
func vodsParseDuration(s string) (time.Duration, error) {
	m := vodsDurationPattern.FindStringSubmatch(s)
	if m == nil || s == "" {
		return 0, fmt.Errorf("duration %q is not of the form 3h2m1s", s)
	}
	var total time.Duration
	for i, unit := range []time.Duration{time.Hour, time.Minute, time.Second} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		// The bound keeps the sum far from overflowing; no video is a
		// million hours long.
		if err != nil || n > 1_000_000 {
			return 0, fmt.Errorf("duration %q is out of range", s)
		}
		total += time.Duration(n) * unit
	}
	return total, nil
}
