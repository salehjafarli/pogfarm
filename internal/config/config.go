package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/salehjafarli/pogfarm/internal/api"
)

// EnvConfigPath names the environment variable that holds the config file
// path when no path is given on the command line. Both binaries use it.
const EnvConfigPath = "POGFARM_CONFIG"

// MinLeaseDuration is the shortest job lease that is accepted. A clipper
// sends several heartbeats per lease, so a shorter one could not be kept.
const MinLeaseDuration = 10 * time.Second

// MinSecretLength is the shortest shared API secret that is accepted.
const MinSecretLength = 16

// SecretPlaceholder is the value config.example.yaml uses for secrets. It is
// public, so it is rejected with its own message instead of being accepted.
const SecretPlaceholder = "change-me"

// Pogfarm is the configuration of the main service.
//
// The yaml tag is the key inside the "pogfarm" section of the config file and
// the env tag is the environment variable that overrides it. Secret fields
// also accept <env>_FILE, naming a file that contains the value.
type Pogfarm struct {
	// ListenAddr is the address the job API listens on, as host:port.
	ListenAddr string `yaml:"listen_addr" env:"POGFARM_LISTEN_ADDR"`
	// DataDir holds the SQLite file and the finished clips.
	DataDir string `yaml:"data_dir" env:"POGFARM_DATA_DIR"`
	// APISecret is the shared secret clippers present to the job API.
	APISecret Secret `yaml:"api_secret" env:"POGFARM_API_SECRET"`

	// Queue configures the job queue.
	Queue Queue `yaml:"queue"`
	// Twitch holds the credentials for the Twitch API.
	Twitch Twitch `yaml:"twitch"`
	// Telegram configures the review bot.
	Telegram Telegram `yaml:"telegram"`

	// TwitchDownloaderPath is the TwitchDownloaderCLI program used to
	// download the chat of a Twitch VOD: a path, or a name looked up in
	// PATH.
	TwitchDownloaderPath string `yaml:"twitch_downloader_path" env:"POGFARM_TWITCH_DOWNLOADER_PATH"`

	// Streamers is the allowlist: the only streamers pogfarm processes. It
	// is set in the config file only.
	Streamers []Streamer `yaml:"streamers"`
	// Discovery configures how VODs of the allowlisted streamers are found.
	Discovery Discovery `yaml:"discovery"`
}

// Streamer is one entry of the allowlist.
type Streamer struct {
	// Platform is the platform the streamer is on, for example "twitch".
	Platform api.Platform `yaml:"platform"`
	// Login is the streamer's account name on the platform, as used in
	// URLs. It is lower-cased when the configuration is loaded.
	Login string `yaml:"login"`
	// Enabled switches the entry on or off. It is on when the key is left
	// out; after loading it is never nil. Use On to read it.
	Enabled *bool `yaml:"enabled"`
}

// On reports whether the entry is switched on. An entry without the enabled
// key is on.
func (s Streamer) On() bool {
	return s.Enabled == nil || *s.Enabled
}

// Discovery is the "discovery" section of the main service's configuration.
type Discovery struct {
	// MaxVODAge is how long after its start a VOD is still recorded. Older
	// VODs are skipped, because the platform deletes VODs after a retention
	// period.
	MaxVODAge time.Duration `yaml:"max_vod_age" env:"POGFARM_DISCOVERY_MAX_VOD_AGE"`
}

// Queue is the "queue" section of the main service's configuration.
type Queue struct {
	// LeaseDuration is how long a claimed job stays with its clipper unless
	// a heartbeat extends the lease.
	LeaseDuration time.Duration `yaml:"lease_duration" env:"POGFARM_QUEUE_LEASE_DURATION"`
}

// Twitch is the "twitch" section of the main service's configuration: the
// credentials of the application registered with Twitch, which pogfarm uses
// to obtain an app access token. Both keys are set, or neither.
type Twitch struct {
	// ClientID is the client id of the registered application.
	ClientID string `yaml:"client_id" env:"POGFARM_TWITCH_CLIENT_ID"`
	// ClientSecret is the client secret of the registered application.
	ClientSecret Secret `yaml:"client_secret" env:"POGFARM_TWITCH_CLIENT_SECRET"`
}

// Telegram is the configuration of the review bot. Both fields are set, or
// neither: with neither, pogfarm runs without the bot.
type Telegram struct {
	// BotToken is the token BotFather issued for the bot.
	BotToken Secret `yaml:"bot_token" env:"POGFARM_TELEGRAM_BOT_TOKEN"`
	// OwnerChatID is the only chat the bot answers. Updates from any other
	// chat are ignored.
	OwnerChatID int64 `yaml:"owner_chat_id" env:"POGFARM_TELEGRAM_OWNER_CHAT_ID"`
}

// Enabled reports whether the bot is configured.
func (t Telegram) Enabled() bool {
	return t.BotToken.IsSet() && t.OwnerChatID != 0
}

// Clipper is the configuration of the render service.
type Clipper struct {
	// PogfarmURL is the base URL of the pogfarm job API.
	PogfarmURL string `yaml:"pogfarm_url" env:"CLIPPER_POGFARM_URL"`
	// APISecret is the shared secret presented to pogfarm. It must match
	// pogfarm's own.
	APISecret Secret `yaml:"api_secret" env:"CLIPPER_API_SECRET"`
	// ScratchDir is where a job's working files live until it finishes.
	ScratchDir string `yaml:"scratch_dir" env:"CLIPPER_SCRATCH_DIR"`
	// Name identifies this clipper in pogfarm's queue and status output.
	// It defaults to the machine's hostname.
	Name string `yaml:"name" env:"CLIPPER_NAME"`
}

// file is the layout of the config file: one section per binary, so a single
// file can configure both on a development machine.
type file struct {
	Pogfarm Pogfarm `yaml:"pogfarm"`
	Clipper Clipper `yaml:"clipper"`
}

func defaults() file {
	return file{
		Pogfarm: Pogfarm{
			ListenAddr: ":8080",
			DataDir:    "data",
			Queue:      Queue{LeaseDuration: 2 * time.Minute},

			TwitchDownloaderPath: "TwitchDownloaderCLI",

			Discovery: Discovery{MaxVODAge: 72 * time.Hour},
		},
		Clipper: Clipper{
			ScratchDir: "scratch",
		},
	}
}

// LoadPogfarm loads and validates the main service's configuration.
//
// path is the config file given on the command line; when empty, the path is
// taken from POGFARM_CONFIG, and when that is empty too, no file is read and
// everything comes from defaults and the environment. Environment variables
// override the file. All validation problems are returned together as a
// *ValidationError.
func LoadPogfarm(path string) (*Pogfarm, error) {
	f := defaults()
	if err := readFile(resolvePath(path), &f); err != nil {
		return nil, err
	}
	cfg := &f.Pogfarm
	problems := applyEnv(cfg)
	problems = append(problems, cfg.validate()...)
	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return cfg, nil
}

// LoadClipper loads and validates the render service's configuration. It
// resolves the file path and applies overrides the same way as LoadPogfarm.
func LoadClipper(path string) (*Clipper, error) {
	f := defaults()
	if err := readFile(resolvePath(path), &f); err != nil {
		return nil, err
	}
	cfg := &f.Clipper
	problems := applyEnv(cfg)
	if cfg.Name == "" {
		if host, err := os.Hostname(); err == nil {
			cfg.Name = host
		}
	}
	cfg.PogfarmURL = strings.TrimRight(cfg.PogfarmURL, "/")
	problems = append(problems, cfg.validate()...)
	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return cfg, nil
}

// String returns the configuration as YAML with secrets replaced by a
// placeholder. It is safe to log.
func (c Pogfarm) String() string { return redactedYAML(c) }

// String returns the configuration as YAML with secrets replaced by a
// placeholder. It is safe to log.
func (c Clipper) String() string { return redactedYAML(c) }

func redactedYAML(v any) string {
	out, err := yaml.Marshal(v)
	if err != nil {
		return "config: cannot render: " + err.Error()
	}
	return strings.TrimRight(string(out), "\n")
}

func (c *Pogfarm) validate() []string {
	var problems []string
	if c.ListenAddr == "" {
		problems = append(problems, "pogfarm.listen_addr (POGFARM_LISTEN_ADDR) must not be empty")
	} else if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		problems = append(problems, fmt.Sprintf(
			"pogfarm.listen_addr (POGFARM_LISTEN_ADDR) must be host:port, for example \":8080\": got %q", c.ListenAddr))
	}
	if c.DataDir == "" {
		problems = append(problems, "pogfarm.data_dir (POGFARM_DATA_DIR) must not be empty")
	}
	problems = append(problems, checkSecret("pogfarm.api_secret", "POGFARM_API_SECRET", c.APISecret)...)
	if c.Queue.LeaseDuration < MinLeaseDuration {
		problems = append(problems, fmt.Sprintf(
			"pogfarm.queue.lease_duration (POGFARM_QUEUE_LEASE_DURATION) must be at least %s: got %s",
			MinLeaseDuration, c.Queue.LeaseDuration))
	}
	problems = append(problems, c.Twitch.validate()...)
	problems = append(problems, c.Telegram.validate()...)

	if c.TwitchDownloaderPath == "" {
		problems = append(problems, "pogfarm.twitch_downloader_path (POGFARM_TWITCH_DOWNLOADER_PATH) must not be empty")
	}

	problems = append(problems, c.validateStreamers()...)
	if c.Discovery.MaxVODAge <= 0 {
		problems = append(problems, fmt.Sprintf(
			"pogfarm.discovery.max_vod_age (POGFARM_DISCOVERY_MAX_VOD_AGE) must be a positive duration such as \"72h\": got %s", c.Discovery.MaxVODAge))
	}
	return problems
}

// validateStreamers checks the allowlist and puts it in its loaded form:
// logins lower-cased and every entry's Enabled set. An empty allowlist is
// valid; pogfarm then processes nobody.
func (c *Pogfarm) validateStreamers() []string {
	var problems []string
	type key struct {
		platform api.Platform
		login    string
	}
	seen := make(map[key]bool)
	for i := range c.Streamers {
		s := &c.Streamers[i]
		name := fmt.Sprintf("pogfarm.streamers[%d]", i)
		s.Login = strings.ToLower(s.Login)
		on := s.On()
		s.Enabled = &on

		ok := true
		switch {
		case s.Platform == "":
			problems = append(problems, fmt.Sprintf("%s.platform is required, for example %q", name, api.PlatformTwitch))
			ok = false
		case s.Platform != api.PlatformTwitch:
			problems = append(problems, fmt.Sprintf(
				"%s.platform is not a platform pogfarm knows: got %q, want %q", name, s.Platform, api.PlatformTwitch))
			ok = false
		}
		switch {
		case s.Login == "":
			problems = append(problems, name+".login is required: the streamer's account name")
			ok = false
		case strings.ContainsFunc(s.Login, func(r rune) bool { return r <= ' ' || r == '/' || r == 0x7f }):
			problems = append(problems, fmt.Sprintf(
				"%s.login must be the account name alone, without spaces or a URL: got %q", name, s.Login))
			ok = false
		}
		if !ok {
			continue
		}
		k := key{s.Platform, s.Login}
		if seen[k] {
			problems = append(problems, fmt.Sprintf(
				"%s repeats the streamer %s/%s: list each streamer once", name, s.Platform, s.Login))
		}
		seen[k] = true
	}
	return problems
}

// validate checks the Twitch credentials without ever putting the secret in
// a message. They are optional as a pair: with neither set there is nothing
// to check, and the Twitch client refuses to be built without them.
func (c *Twitch) validate() []string {
	const (
		idKey, idEnv         = "pogfarm.twitch.client_id", "POGFARM_TWITCH_CLIENT_ID"
		secretKey, secretEnv = "pogfarm.twitch.client_secret", "POGFARM_TWITCH_CLIENT_SECRET"
	)
	var problems []string
	switch {
	case c.ClientID == SecretPlaceholder:
		problems = append(problems, fmt.Sprintf(
			"%s is still the example placeholder: set %s to the client id of your Twitch application", idKey, idEnv))
	case c.ClientID == "" && c.ClientSecret.IsSet():
		problems = append(problems, fmt.Sprintf(
			"%s is required when %s is set: set %s", idKey, secretKey, idEnv))
	}
	switch {
	case c.ClientSecret.Reveal() == SecretPlaceholder:
		problems = append(problems, fmt.Sprintf(
			"%s is still the example placeholder: set %s, or %s_FILE, to the client secret of your Twitch application", secretKey, secretEnv, secretEnv))
	case !c.ClientSecret.IsSet() && c.ClientID != "":
		problems = append(problems, fmt.Sprintf(
			"%s is required when %s is set: set %s, or %s_FILE to a file containing it", secretKey, idKey, secretEnv, secretEnv))
	}
	return problems
}

// validate never puts the token in a message.
func (t Telegram) validate() []string {
	const (
		tokenKey, tokenEnv = "pogfarm.telegram.bot_token", "POGFARM_TELEGRAM_BOT_TOKEN"
		ownerKey, ownerEnv = "pogfarm.telegram.owner_chat_id", "POGFARM_TELEGRAM_OWNER_CHAT_ID"
	)
	var problems []string
	switch {
	case t.BotToken.Reveal() == SecretPlaceholder:
		problems = append(problems, fmt.Sprintf(
			"%s is still the example placeholder: set %s, or %s_FILE, to the token from BotFather", tokenKey, tokenEnv, tokenEnv))
	case !t.BotToken.IsSet() && t.OwnerChatID != 0:
		problems = append(problems, fmt.Sprintf(
			"%s is required when %s is set: set %s, or %s_FILE to a file containing it", tokenKey, ownerKey, tokenEnv, tokenEnv))
	}
	if t.BotToken.IsSet() && t.OwnerChatID == 0 {
		problems = append(problems, fmt.Sprintf(
			"%s (%s) is required when %s is set: the id of the only chat the bot answers", ownerKey, ownerEnv, tokenKey))
	}
	return problems
}

var clipperNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func (c *Clipper) validate() []string {
	var problems []string
	switch u, err := url.Parse(c.PogfarmURL); {
	case c.PogfarmURL == "":
		problems = append(problems, "clipper.pogfarm_url (CLIPPER_POGFARM_URL) is required, for example \"http://pogfarm:8080\"")
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		problems = append(problems, fmt.Sprintf(
			"clipper.pogfarm_url (CLIPPER_POGFARM_URL) must be an http or https URL with a host: got %q", c.PogfarmURL))
	}
	problems = append(problems, checkSecret("clipper.api_secret", "CLIPPER_API_SECRET", c.APISecret)...)
	if c.ScratchDir == "" {
		problems = append(problems, "clipper.scratch_dir (CLIPPER_SCRATCH_DIR) must not be empty")
	}
	switch {
	case c.Name == "":
		problems = append(problems, "clipper.name (CLIPPER_NAME) is required: the hostname could not be read to use as a default")
	case !clipperNamePattern.MatchString(c.Name):
		problems = append(problems, fmt.Sprintf(
			"clipper.name (CLIPPER_NAME) must be 1 to 63 letters, digits, dots, dashes or underscores, starting with a letter or digit: got %q", c.Name))
	}
	return problems
}

// checkSecret validates a shared secret without ever putting its value in a
// message.
func checkSecret(key, env string, s Secret) []string {
	switch n := len(s.Reveal()); {
	case n == 0:
		return []string{fmt.Sprintf("%s is required: set %s, or %s_FILE to a file containing it", key, env, env)}
	case s.Reveal() == SecretPlaceholder:
		return []string{fmt.Sprintf("%s is still the example placeholder: set %s, or %s_FILE, to a real secret of at least %d characters", key, env, env, MinSecretLength)}
	case n < MinSecretLength:
		return []string{fmt.Sprintf("%s (%s) is too short: %d characters, need at least %d", key, env, n, MinSecretLength)}
	}
	return nil
}

// ValidationError reports every configuration problem found, so they can all
// be fixed in one go.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	noun := "problems"
	if len(e.Problems) == 1 {
		noun = "problem"
	}
	return fmt.Sprintf("invalid configuration (%d %s):\n  - %s",
		len(e.Problems), noun, strings.Join(e.Problems, "\n  - "))
}
