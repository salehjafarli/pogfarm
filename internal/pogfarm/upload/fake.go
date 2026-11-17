package upload

import (
	"context"
	"fmt"
	"sync"
)

// Fake is a Target for tests. It keeps the posts it made in memory and
// records every call. It does not implement Checker; CheckingFake does.
// It is safe for concurrent use.
type Fake struct {
	name string

	mu    sync.Mutex
	calls []Post
	posts map[int64]FakePost // by clip id
	fails []error
	hang  []chan Post
	gate  chan struct{}
	enter chan Post
	n     int
}

// FakePost is a post a Fake made.
type FakePost struct {
	ExternalID string
	URL        string
	Post       Post
}

// NewFake returns a fake target with the given name.
func NewFake(name string) *Fake {
	return &Fake{name: name, posts: make(map[int64]FakePost)}
}

// Name returns the fake's name.
func (f *Fake) Name() string { return f.name }

// FailNext makes the next calls of Upload return errs, one per call, in
// order, without posting. Wrap one with Permanent or Unconfirmed to make
// it so.
func (f *Fake) FailNext(errs ...error) {
	f.mu.Lock()
	f.fails = append(f.fails, errs...)
	f.mu.Unlock()
}

// HangAfterPost makes the next call of Upload post the clip, send the post
// on the returned channel and then wait until its context ends, returning
// its error: an upload that went through but whose result never reached
// pogfarm, as when pogfarm stops or the call times out.
func (f *Fake) HangAfterPost() <-chan Post {
	ch := make(chan Post, 1)
	f.mu.Lock()
	f.hang = append(f.hang, ch)
	f.mu.Unlock()
	return ch
}

// Hold makes every later call of Upload send its post on the returned
// channel and then wait, before posting, until release is called or its
// context ends. It is for tests of a stuck target.
func (f *Fake) Hold() (entered <-chan Post, release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = make(chan struct{})
	f.enter = make(chan Post, 16)
	gate := f.gate
	var once sync.Once
	return f.enter, func() { once.Do(func() { close(gate) }) }
}

// Upload records the call and, unless told otherwise, posts p.
func (f *Fake) Upload(ctx context.Context, p Post) (string, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, p)
	gate, enter := f.gate, f.enter
	f.mu.Unlock()

	if gate != nil {
		enter <- p
		select {
		case <-gate:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}

	f.mu.Lock()
	if len(f.fails) > 0 {
		err := f.fails[0]
		f.fails = f.fails[1:]
		f.mu.Unlock()
		return "", "", err
	}
	f.n++
	fp := FakePost{
		ExternalID: fmt.Sprintf("%s-%d", f.name, f.n),
		Post:       p,
	}
	fp.URL = "https://" + f.name + ".example/" + fp.ExternalID
	f.posts[p.ClipID] = fp
	var hang chan Post
	if len(f.hang) > 0 {
		hang, f.hang = f.hang[0], f.hang[1:]
	}
	f.mu.Unlock()

	if hang != nil {
		hang <- p
		<-ctx.Done()
		return "", "", ctx.Err()
	}
	return fp.ExternalID, fp.URL, nil
}

// Calls returns the posts Upload was called with, in order.
func (f *Fake) Calls() []Post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Post(nil), f.calls...)
}

// Posts returns the posts the fake made, by clip id.
func (f *Fake) Posts() map[int64]FakePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[int64]FakePost, len(f.posts))
	for k, v := range f.posts {
		out[k] = v
	}
	return out
}

// CheckingFake is a Fake that also implements Checker.
type CheckingFake struct {
	*Fake

	mu       sync.Mutex
	finds    []Post
	findErrs []error
}

// NewCheckingFake returns a fake target with Checker.
func NewCheckingFake(name string) *CheckingFake {
	return &CheckingFake{Fake: NewFake(name)}
}

// FailNextFind makes the next calls of Find return errs, one per call.
func (c *CheckingFake) FailNextFind(errs ...error) {
	c.mu.Lock()
	c.findErrs = append(c.findErrs, errs...)
	c.mu.Unlock()
}

// Find reports whether the fake has a post of p.ClipID.
func (c *CheckingFake) Find(_ context.Context, p Post) (string, string, bool, error) {
	c.mu.Lock()
	c.finds = append(c.finds, p)
	if len(c.findErrs) > 0 {
		err := c.findErrs[0]
		c.findErrs = c.findErrs[1:]
		c.mu.Unlock()
		return "", "", false, err
	}
	c.mu.Unlock()
	fp, ok := c.Posts()[p.ClipID]
	return fp.ExternalID, fp.URL, ok, nil
}

// Finds returns the posts Find was called with, in order.
func (c *CheckingFake) Finds() []Post {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Post(nil), c.finds...)
}
