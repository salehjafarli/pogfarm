// Package source is the boundary between pogfarm and the platforms that host
// streams. Everything specific to one platform lives in a subpackage that
// implements Source; the rest of pogfarm only sees the common types defined
// here, so chat scanning and scheduling never know where their data came
// from.
//
// A new platform is added as a sibling subpackage and registered in a
// Registry under its platform name. Nothing in this package may name a
// particular platform.
package source

import (
	"context"
	"errors"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
)

// ErrVODNotFound is returned (wrapped) by Chat and ViewerClips when the
// platform no longer has the VOD, for example because it was deleted.
var ErrVODNotFound = errors.New("source: vod not found")

// ErrStreamerNotFound is returned (wrapped) by RecentVODs when the platform
// has no account with the given login.
var ErrStreamerNotFound = errors.New("source: streamer not found")

// ErrViewerClipsUnsupported is returned by ViewerClips when the source does
// not implement ViewerClipLister.
var ErrViewerClipsUnsupported = errors.New("source: viewer clips not supported")

// VOD is one recorded stream.
type VOD struct {
	// Platform is the platform the VOD is hosted on.
	Platform api.Platform
	// ID is the platform's own identifier of the VOD. Together with
	// Platform it identifies the VOD.
	ID string
	// Streamer is whose VOD this is.
	Streamer api.Streamer
	// StreamerID is the platform's permanent identifier of the streamer's
	// account, which stays the same when the login changes. RecentVODs
	// sets it.
	StreamerID string
	// Title is the stream title.
	Title string
	// StartedAt is when the stream started.
	StartedAt time.Time
	// Duration is the length of the VOD. It is still growing while Ended is
	// false.
	Duration time.Duration
	// Ended reports whether the stream has ended. The chat and duration of
	// a VOD are complete only once it has.
	Ended bool
}

// ChatMessage is one chat message of a VOD, in the format every platform's
// chat is converted to.
type ChatMessage struct {
	// Offset is when the message was sent, measured from the start of the
	// VOD.
	Offset time.Duration
	// User is the name of the sender.
	User string
	// Text is the message as plain text. Emotes appear in it by name.
	Text string
	// Emotes are the names of the emotes used in the message. It is nil
	// when there are none or the platform does not report them.
	Emotes []string
}

// ViewerClip is a clip of a VOD made by a viewer.
type ViewerClip struct {
	// Offset is where the clip starts, measured from the start of the VOD.
	Offset time.Duration
	// Duration is the length of the clip.
	Duration time.Duration
	// Views is how many times the clip has been viewed.
	Views int
	// Title is the title the viewer gave the clip.
	Title string
}

// Source is one platform. Implementations are safe for concurrent use and
// stop early with the context's error when ctx is cancelled.
type Source interface {
	// Platform returns the platform name this source serves. It is the
	// Platform of every VOD the source returns.
	Platform() api.Platform

	// RecentVODs lists the recent VODs of the streamer with the given
	// login, newest first. A stream that is still live is included with
	// Ended false. The error wraps ErrStreamerNotFound when the platform
	// has no account with that login.
	RecentVODs(ctx context.Context, login string) ([]VOD, error)

	// Chat returns the whole chat of a VOD, ordered by offset.
	Chat(ctx context.Context, vod VOD) ([]ChatMessage, error)
}

// ViewerClipLister is the optional capability of a Source to list the clips
// viewers made of a VOD. Use ViewerClips to call it on any Source.
type ViewerClipLister interface {
	// ViewerClips lists the viewer-made clips of a VOD, ordered by offset.
	ViewerClips(ctx context.Context, vod VOD) ([]ViewerClip, error)
}

// ViewerClips lists the viewer-made clips of a VOD if s can list them, and
// returns ErrViewerClipsUnsupported otherwise.
func ViewerClips(ctx context.Context, s Source, vod VOD) ([]ViewerClip, error) {
	l, ok := s.(ViewerClipLister)
	if !ok {
		return nil, ErrViewerClipsUnsupported
	}
	return l.ViewerClips(ctx, vod)
}
