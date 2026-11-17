package upload

import (
	"context"
	"errors"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// Post is one clip as it is sent to a target platform.
type Post struct {
	ClipID      int64
	FilePath    string // absolute path of the clip file
	Title       string
	Description string
	Tags        []string
}

// Target is one platform clips are posted to.
//
// Name is the name the config file and the uploads table use for the
// target, for example "youtube".
//
// Upload posts p and returns the platform's id of the new post and a link
// to it. How the uploader treats an error:
//
//   - Permanent(err): the post was not made and retrying cannot help (the
//     file is rejected, the account is banned). The upload fails at once
//     and the owner is told.
//   - Unconfirmed(err): the target cannot tell whether the post was made
//     (the connection broke after the file was sent). The upload is settled
//     like one cut off by a crash: through Checker if the target has it,
//     otherwise by the owner.
//   - any other error: the post was not made and a later attempt may work
//     (rate limit, server error, network error before the upload). It is
//     retried with backoff up to the attempt limit.
//
// A call that outlives the per-attempt timeout counts as Unconfirmed.
// Upload must stop promptly when ctx is cancelled.
type Target interface {
	Name() string
	Upload(ctx context.Context, p Post) (externalID, url string, err error)
}

// Checker is an optional interface of a Target that can look up whether a
// clip was posted. The uploader uses it to settle an upload whose outcome
// is unknown, because pogfarm stopped or the call timed out before the
// result was recorded. found is true when the platform has a post of
// p.ClipID; err means the lookup itself failed, and it is tried again on a
// later pass.
type Checker interface {
	Find(ctx context.Context, p Post) (externalID, url string, found bool, err error)
}

// Composer builds the title, description and tags of a clip's post to one
// target.
type Composer func(ctx context.Context, clip store.Clip, target string) (title, description string, tags []string, err error)

// Notifier tells the owner about posts and failures, for example through
// the review bot. review.Bot's Send method fits NotifyFunc.
type Notifier interface {
	Notify(ctx context.Context, text string) error
}

// NotifyFunc adapts a function to Notifier.
type NotifyFunc func(ctx context.Context, text string) error

// Notify calls f.
func (f NotifyFunc) Notify(ctx context.Context, text string) error { return f(ctx, text) }

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

type unconfirmedError struct{ err error }

func (e unconfirmedError) Error() string { return e.err.Error() }
func (e unconfirmedError) Unwrap() error { return e.err }

// Permanent marks err as an error retrying cannot fix. Nil stays nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether err, or an error it wraps, was marked by
// Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// Unconfirmed marks err as one after which the post may or may not exist.
// Nil stays nil.
func Unconfirmed(err error) error {
	if err == nil {
		return nil
	}
	return unconfirmedError{err}
}

// IsUnconfirmed reports whether err, or an error it wraps, was marked by
// Unconfirmed.
func IsUnconfirmed(err error) bool {
	var u unconfirmedError
	return errors.As(err, &u)
}
