package twitch

import (
	"context"
	"errors"
	"fmt"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
)

// Source is Twitch as a source.Source. It puts the parts of this package
// together: VODs and account lookups come from a VODLister, viewer clips
// from a ClipLister, both over one Client, and chat from a ChatFetcher. It
// is safe for concurrent use.
type Source struct {
	vods  *VODLister
	clips *ClipLister
	chat  *ChatFetcher
}

var (
	_ source.Source           = (*Source)(nil)
	_ source.ViewerClipLister = (*Source)(nil)
	_ source.StreamerLookup   = (*Source)(nil)
)

// NewSource returns the source that makes its Helix requests through c and
// fetches chat with chat. Neither may be nil.
func NewSource(c *Client, chat *ChatFetcher) (*Source, error) {
	if c == nil || chat == nil {
		return nil, errors.New("twitch: a source needs a client and a chat fetcher")
	}
	return &Source{vods: NewVODLister(c), clips: NewClipLister(c), chat: chat}, nil
}

// NewSourceFromConfig builds the source from pogfarm's configuration: the
// client from the twitch section, and a chat fetcher that runs
// twitch_downloader_path and downloads into data_dir. It fails when the
// Twitch credentials are not set.
func NewSourceFromConfig(cfg *config.Pogfarm) (*Source, error) {
	if cfg == nil {
		return nil, errors.New("twitch: no configuration")
	}
	c, err := NewClient(Options{ClientID: cfg.Twitch.ClientID, ClientSecret: cfg.Twitch.ClientSecret})
	if err != nil {
		return nil, fmt.Errorf("twitch: build source: %w", err)
	}
	return NewSource(c, &ChatFetcher{Binary: cfg.TwitchDownloaderPath, Dir: cfg.DataDir})
}

// Platform implements source.Source. It is api.PlatformTwitch.
func (s *Source) Platform() api.Platform { return api.PlatformTwitch }

// RecentVODs implements source.Source; see VODLister.RecentVODs.
func (s *Source) RecentVODs(ctx context.Context, login string) ([]source.VOD, error) {
	return s.vods.RecentVODs(ctx, login)
}

// Chat implements source.Source; see ChatFetcher.Chat.
func (s *Source) Chat(ctx context.Context, vod source.VOD) ([]source.ChatMessage, error) {
	return s.chat.Chat(ctx, vod)
}

// ViewerClips implements source.ViewerClipLister; see
// ClipLister.ViewerClips.
func (s *Source) ViewerClips(ctx context.Context, vod source.VOD) ([]source.ViewerClip, error) {
	return s.clips.ViewerClips(ctx, vod)
}

// LookupStreamer implements source.StreamerLookup; see
// VODLister.LookupStreamer.
func (s *Source) LookupStreamer(ctx context.Context, login string) (source.Account, error) {
	return s.vods.LookupStreamer(ctx, login)
}
