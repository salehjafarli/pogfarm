package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// EnvConfigPath names the environment variable that holds the config file
// path when no path is given on the command line. Both binaries use it.
const EnvConfigPath = "POGFARM_CONFIG"

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
	// Telegram configures the review bot.
	Telegram Telegram `yaml:"telegram"`
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
	problems = append(problems, c.Telegram.validate()...)
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
