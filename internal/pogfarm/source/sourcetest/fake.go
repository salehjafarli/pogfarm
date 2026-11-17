// Package sourcetest provides in-memory implementations of source.Source for
// tests. They hold whatever VODs, chat and viewer clips a test gives them and
// touch neither the network nor the disk.
package sourcetest

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
)

// Fake is an in-memory source.Source without the viewer-clip capability. Use
// FakeWithClips for a source that has it. A Fake is safe for concurrent use.
//
// Data handed to a Fake and data it returns are copied, so a test can keep
// changing its own slices without affecting the Fake and the other way round.
type Fake struct {
	platform api.Platform

	mu    sync.Mutex
	vods  map[string][]source.VOD // by streamer login, as added
	chats map[string][]source.ChatMessage
	clips map[string][]source.ViewerClip
	err   error
	calls []Call
}

// Call records one call made to a fake.
type Call struct {
	// Method is the name of the method called: "RecentVODs", "Chat" or
	// "ViewerClips".
	Method string
	// Arg is the streamer login for RecentVODs and the VOD ID for the
	// others.
	Arg string
}

// New returns an empty fake that serves the given platform name.
func New(platform api.Platform) *Fake {
	return &Fake{
		platform: platform,
		vods:     make(map[string][]source.VOD),
		chats:    make(map[string][]source.ChatMessage),
		clips:    make(map[string][]source.ViewerClip),
	}
}

// AddVOD adds or replaces a VOD, keyed by its ID, under its streamer's login.
// The VOD's Platform is set to the fake's own. A VOD has an empty chat and no
// viewer clips until SetChat and SetViewerClips say otherwise.
func (f *Fake) AddVOD(vod source.VOD) {
	vod.Platform = f.platform

	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeLocked(vod.ID)
	login := vod.Streamer.Login
	f.vods[login] = append(f.vods[login], vod)
}

// RemoveVOD removes a VOD with its chat and viewer clips, as if the platform
// had deleted it. Later Chat and ViewerClips calls for it fail with
// source.ErrVODNotFound.
func (f *Fake) RemoveVOD(vodID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeLocked(vodID)
	delete(f.chats, vodID)
	delete(f.clips, vodID)
}

func (f *Fake) removeLocked(vodID string) {
	for login, vods := range f.vods {
		f.vods[login] = slices.DeleteFunc(vods, func(v source.VOD) bool { return v.ID == vodID })
	}
}

// SetChat sets the chat of a VOD. The messages are stored ordered by offset;
// messages with the same offset keep the order given.
func (f *Fake) SetChat(vodID string, messages []source.ChatMessage) {
	messages = cloneChat(messages)
	slices.SortStableFunc(messages, func(a, b source.ChatMessage) int {
		return cmp.Compare(a.Offset, b.Offset)
	})

	f.mu.Lock()
	defer f.mu.Unlock()
	f.chats[vodID] = messages
}

// SetViewerClips sets the viewer clips of a VOD. They are stored ordered by
// offset. Only a FakeWithClips returns them.
func (f *Fake) SetViewerClips(vodID string, clips []source.ViewerClip) {
	clips = slices.Clone(clips)
	slices.SortStableFunc(clips, func(a, b source.ViewerClip) int {
		return cmp.Compare(a.Offset, b.Offset)
	})

	f.mu.Lock()
	defer f.mu.Unlock()
	f.clips[vodID] = clips
}

// SetError makes every later operation fail with err. Pass nil to make them
// work again.
func (f *Fake) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Calls returns the calls made so far, in order, including failed ones.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Platform implements source.Source.
func (f *Fake) Platform() api.Platform { return f.platform }

// RecentVODs implements source.Source. It returns the streamer's VODs newest
// first, and none for a login it has no VODs for.
func (f *Fake) RecentVODs(ctx context.Context, login string) ([]source.VOD, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.beginLocked(ctx, "RecentVODs", login); err != nil {
		return nil, err
	}
	vods := slices.Clone(f.vods[login])
	slices.SortStableFunc(vods, func(a, b source.VOD) int {
		return b.StartedAt.Compare(a.StartedAt)
	})
	return vods, nil
}

// Chat implements source.Source. It fails with source.ErrVODNotFound for a
// VOD that was not added.
func (f *Fake) Chat(ctx context.Context, vod source.VOD) ([]source.ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.beginLocked(ctx, "Chat", vod.ID); err != nil {
		return nil, err
	}
	if !f.hasLocked(vod.ID) {
		return nil, fmt.Errorf("%w: %s", source.ErrVODNotFound, vod.ID)
	}
	return cloneChat(f.chats[vod.ID]), nil
}

func (f *Fake) viewerClips(ctx context.Context, vod source.VOD) ([]source.ViewerClip, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.beginLocked(ctx, "ViewerClips", vod.ID); err != nil {
		return nil, err
	}
	if !f.hasLocked(vod.ID) {
		return nil, fmt.Errorf("%w: %s", source.ErrVODNotFound, vod.ID)
	}
	return slices.Clone(f.clips[vod.ID]), nil
}

// beginLocked records the call and returns the error it must fail with, if
// any: the context's error first, then the one from SetError.
func (f *Fake) beginLocked(ctx context.Context, method, arg string) error {
	f.calls = append(f.calls, Call{Method: method, Arg: arg})
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.err
}

func (f *Fake) hasLocked(vodID string) bool {
	for _, vods := range f.vods {
		if slices.ContainsFunc(vods, func(v source.VOD) bool { return v.ID == vodID }) {
			return true
		}
	}
	return false
}

// FakeWithClips is a Fake that also implements source.ViewerClipLister.
type FakeWithClips struct {
	*Fake
}

// NewWithClips returns an empty fake with the viewer-clip capability that
// serves the given platform name.
func NewWithClips(platform api.Platform) *FakeWithClips {
	return &FakeWithClips{Fake: New(platform)}
}

// ViewerClips implements source.ViewerClipLister. It fails with
// source.ErrVODNotFound for a VOD that was not added.
func (f *FakeWithClips) ViewerClips(ctx context.Context, vod source.VOD) ([]source.ViewerClip, error) {
	return f.viewerClips(ctx, vod)
}

// cloneChat copies the messages and their emote lists.
func cloneChat(messages []source.ChatMessage) []source.ChatMessage {
	out := slices.Clone(messages)
	for i := range out {
		out[i].Emotes = slices.Clone(out[i].Emotes)
	}
	return out
}
