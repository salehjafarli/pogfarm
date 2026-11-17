package config

import (
	"fmt"
	"time"
)

// Upload is the "upload" section of the main service's configuration: the
// loop that posts approved clips to the upload targets.
type Upload struct {
	// Targets are the places clips are posted to. File only. With no
	// enabled target nothing is posted, and approving a clip creates no
	// upload rows.
	Targets []UploadTarget `yaml:"targets"`
	// MaxAttempts is how many uploads of one clip to one target are started
	// before a clip whose uploads keep failing is given up.
	MaxAttempts int `yaml:"max_attempts" env:"POGFARM_UPLOAD_MAX_ATTEMPTS"`
	// RetryBackoff is the wait before the first retry of a failed upload.
	// It doubles with every further failure, up to MaxRetryBackoff.
	RetryBackoff time.Duration `yaml:"retry_backoff" env:"POGFARM_UPLOAD_RETRY_BACKOFF"`
	// MaxRetryBackoff caps the wait between two attempts.
	MaxRetryBackoff time.Duration `yaml:"max_retry_backoff" env:"POGFARM_UPLOAD_MAX_RETRY_BACKOFF"`
	// PollInterval is how long the loop waits between two passes over the
	// pending uploads.
	PollInterval time.Duration `yaml:"poll_interval" env:"POGFARM_UPLOAD_POLL_INTERVAL"`
	// AttemptTimeout bounds one call to a target's upload.
	AttemptTimeout time.Duration `yaml:"attempt_timeout" env:"POGFARM_UPLOAD_ATTEMPT_TIMEOUT"`
}

// UploadTarget is one entry of upload.targets.
type UploadTarget struct {
	// Name names the target, for example "youtube". It is what
	// uploads.target_platform holds. Lower-case letters, digits, '_' and
	// '-'; unique in the list.
	Name string `yaml:"name"`
	// Enabled switches the target on or off. It is on when the key is left
	// out. Use On to read it.
	Enabled *bool `yaml:"enabled"`
	// DailyCap is the most posts made to the target in any 24 hours
	// (a rolling window, not a calendar day).
	DailyCap int `yaml:"daily_cap"`
	// MinSpacing is the least time between two posts to the target. Zero
	// means no spacing.
	MinSpacing time.Duration `yaml:"min_spacing"`
}

// On reports whether the target is switched on. A target without the
// enabled key is on.
func (t UploadTarget) On() bool {
	return t.Enabled == nil || *t.Enabled
}

// EnabledTargets returns the names of the enabled targets, in the order of
// the list. Approval creates one upload row per name.
func (c Upload) EnabledTargets() []string {
	var names []string
	for _, t := range c.Targets {
		if t.On() {
			names = append(names, t.Name)
		}
	}
	return names
}

// defaultUpload is the upload section when nothing is configured: no
// targets.
func defaultUpload() Upload {
	return Upload{
		MaxAttempts:     5,
		RetryBackoff:    5 * time.Minute,
		MaxRetryBackoff: 6 * time.Hour,
		PollInterval:    time.Minute,
		AttemptTimeout:  30 * time.Minute,
	}
}

func (c Upload) validate() []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	positive := func(key, env string, d time.Duration) {
		if d <= 0 {
			add("pogfarm.upload.%s (%s) must be a positive duration: got %s", key, env, d)
		}
	}
	if c.MaxAttempts < 1 {
		add("pogfarm.upload.max_attempts (POGFARM_UPLOAD_MAX_ATTEMPTS) must be at least 1: got %d", c.MaxAttempts)
	}
	positive("retry_backoff", "POGFARM_UPLOAD_RETRY_BACKOFF", c.RetryBackoff)
	if c.MaxRetryBackoff < c.RetryBackoff {
		add("pogfarm.upload.max_retry_backoff (POGFARM_UPLOAD_MAX_RETRY_BACKOFF) must be at least retry_backoff (%s): got %s",
			c.RetryBackoff, c.MaxRetryBackoff)
	}
	positive("poll_interval", "POGFARM_UPLOAD_POLL_INTERVAL", c.PollInterval)
	positive("attempt_timeout", "POGFARM_UPLOAD_ATTEMPT_TIMEOUT", c.AttemptTimeout)

	seen := make(map[string]bool, len(c.Targets))
	for i, t := range c.Targets {
		key := fmt.Sprintf("pogfarm.upload.targets[%d]", i)
		switch {
		case t.Name == "":
			add("%s.name is required, for example %q", key, "youtube")
		case !validTargetName(t.Name):
			add("%s.name %q must be lower-case letters, digits, '_' or '-'", key, t.Name)
		case seen[t.Name]:
			add("%s.name %q is listed more than once", key, t.Name)
		}
		seen[t.Name] = true
		if t.DailyCap < 1 {
			add("%s.daily_cap must be at least 1: got %d", key, t.DailyCap)
		}
		if t.MinSpacing < 0 {
			add("%s.min_spacing must not be negative: got %s", key, t.MinSpacing)
		}
	}
	return problems
}

func validTargetName(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return s != ""
}
