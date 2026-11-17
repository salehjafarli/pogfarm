package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	goodSecret  = "correct-horse-battery-staple"
	otherSecret = "another-long-enough-secret"
)

// allEnv lists every variable the loader reads, so tests start from a clean
// slate whatever the developer's shell has set.
var allEnv = []string{
	EnvConfigPath,
	"POGFARM_LISTEN_ADDR", "POGFARM_DATA_DIR", "POGFARM_API_SECRET", "POGFARM_API_SECRET_FILE",
	"POGFARM_TELEGRAM_BOT_TOKEN", "POGFARM_TELEGRAM_BOT_TOKEN_FILE", "POGFARM_TELEGRAM_OWNER_CHAT_ID",
	"CLIPPER_POGFARM_URL", "CLIPPER_API_SECRET", "CLIPPER_API_SECRET_FILE", "CLIPPER_SCRATCH_DIR", "CLIPPER_NAME",
	"POGFARM_TWITCH_CLIENT_ID", "POGFARM_TWITCH_CLIENT_SECRET", "POGFARM_TWITCH_CLIENT_SECRET_FILE",
	"POGFARM_QUEUE_LEASE_DURATION",
	"POGFARM_TWITCH_DOWNLOADER_PATH",
	"POGFARM_MAX_CLIP_BYTES",
}

// cleanEnv blanks every variable for the duration of the test. The loader
// treats an empty variable as unset.
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, name := range allEnv {
		t.Setenv(name, "")
	}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func problemsOf(t *testing.T, err error) []string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
	return ve.Problems
}

func TestPogfarmDefaultsWithEnvOnly(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)

	cfg, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8080" || cfg.DataDir != "data" {
		t.Errorf("defaults not applied: %+v", *cfg)
	}
	if cfg.APISecret.Reveal() != goodSecret {
		t.Error("secret not taken from the environment")
	}
}

func TestFileValuesAreLoaded(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, "config.yaml", `
pogfarm:
  listen_addr: "127.0.0.1:9000"
  data_dir: /var/lib/pogfarm
  api_secret: `+goodSecret+`
clipper:
  pogfarm_url: https://farm.example/
  scratch_dir: /tmp/clips
  name: node-a
  api_secret: `+goodSecret+`
`)

	p, err := LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.ListenAddr != "127.0.0.1:9000" || p.DataDir != "/var/lib/pogfarm" || p.APISecret.Reveal() != goodSecret {
		t.Errorf("pogfarm section not loaded: %+v", *p)
	}

	c, err := LoadClipper(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.PogfarmURL != "https://farm.example" {
		t.Errorf("trailing slash should be trimmed, got %q", c.PogfarmURL)
	}
	if c.ScratchDir != "/tmp/clips" || c.Name != "node-a" || c.APISecret.Reveal() != goodSecret {
		t.Errorf("clipper section not loaded: %+v", *c)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, "config.yaml", `
pogfarm:
  listen_addr: ":9000"
  data_dir: from-file
  api_secret: `+goodSecret+`
`)
	t.Setenv("POGFARM_DATA_DIR", "from-env")
	t.Setenv("POGFARM_API_SECRET", otherSecret)

	cfg, err := LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "from-env" {
		t.Errorf("env should override file: data_dir = %q", cfg.DataDir)
	}
	if cfg.APISecret.Reveal() != otherSecret {
		t.Error("env should override a secret from the file")
	}
	if cfg.ListenAddr != ":9000" {
		t.Errorf("file value without an env override should stay: listen_addr = %q", cfg.ListenAddr)
	}
}

func TestSecretFromFile(t *testing.T) {
	cleanEnv(t)
	secretPath := writeFile(t, "api_secret", goodSecret+"\n")
	cfgPath := writeFile(t, "config.yaml", "pogfarm:\n  api_secret: "+otherSecret+"\n")
	t.Setenv("POGFARM_API_SECRET_FILE", secretPath)

	cfg, err := LoadPogfarm(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APISecret.Reveal() != goodSecret {
		t.Error("secret file should be used, without its trailing newline, and should override the config file")
	}
}

func TestSecretFileProblems(t *testing.T) {
	t.Run("both set", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("POGFARM_API_SECRET", goodSecret)
		t.Setenv("POGFARM_API_SECRET_FILE", writeFile(t, "s", otherSecret))
		_, err := LoadPogfarm("")
		got := strings.Join(problemsOf(t, err), "\n")
		if !strings.Contains(got, "POGFARM_API_SECRET and POGFARM_API_SECRET_FILE are both set") {
			t.Errorf("unexpected problems: %s", got)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("POGFARM_API_SECRET_FILE", filepath.Join(t.TempDir(), "nope"))
		_, err := LoadPogfarm("")
		got := strings.Join(problemsOf(t, err), "\n")
		if !strings.Contains(got, "POGFARM_API_SECRET_FILE: cannot read secret file") {
			t.Errorf("unexpected problems: %s", got)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("POGFARM_API_SECRET_FILE", writeFile(t, "s", "\n"))
		_, err := LoadPogfarm("")
		got := strings.Join(problemsOf(t, err), "\n")
		if !strings.Contains(got, "is empty") {
			t.Errorf("unexpected problems: %s", got)
		}
	})
}

func TestConfigPathResolution(t *testing.T) {
	fromEnv := writeFile(t, "env.yaml", "pogfarm:\n  data_dir: via-env-path\n")
	explicit := writeFile(t, "flag.yaml", "pogfarm:\n  data_dir: via-flag-path\n")

	t.Run("POGFARM_CONFIG is used when no path is given", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("POGFARM_API_SECRET", goodSecret)
		t.Setenv(EnvConfigPath, fromEnv)
		cfg, err := LoadPogfarm("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DataDir != "via-env-path" {
			t.Errorf("data_dir = %q", cfg.DataDir)
		}
	})
	t.Run("explicit path wins over POGFARM_CONFIG", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("POGFARM_API_SECRET", goodSecret)
		t.Setenv(EnvConfigPath, fromEnv)
		cfg, err := LoadPogfarm(explicit)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DataDir != "via-flag-path" {
			t.Errorf("data_dir = %q", cfg.DataDir)
		}
	})
	t.Run("a named file that does not exist is an error", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("POGFARM_API_SECRET", goodSecret)
		_, err := LoadPogfarm(filepath.Join(t.TempDir(), "missing.yaml"))
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			t.Errorf("want a not-exist error, got %v", err)
		}
	})
}

func TestUnknownKeyIsAnError(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	path := writeFile(t, "config.yaml", "pogfarm:\n  listen_adress: \":9000\"\n")
	_, err := LoadPogfarm(path)
	if err == nil || !strings.Contains(err.Error(), "listen_adress") {
		t.Errorf("a misspelled key should be reported by name, got %v", err)
	}
}

func TestEmptyFileIsAccepted(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	for _, content := range []string{"", "# only a comment\n"} {
		if _, err := LoadPogfarm(writeFile(t, "config.yaml", content)); err != nil {
			t.Errorf("content %q: %v", content, err)
		}
	}
}

func TestValidationListsEveryProblem(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, "config.yaml", `
pogfarm:
  listen_addr: "no-port"
  data_dir: ""
clipper:
  pogfarm_url: "ftp://farm"
  scratch_dir: ""
  name: "bad name!"
  api_secret: short
`)

	_, err := LoadPogfarm(path)
	pProblems := problemsOf(t, err)
	if len(pProblems) != 3 {
		t.Errorf("pogfarm: want 3 problems, got %d:\n%s", len(pProblems), strings.Join(pProblems, "\n"))
	}
	msg := err.Error()
	for _, want := range []string{"3 problems", "pogfarm.listen_addr", "pogfarm.data_dir", "pogfarm.api_secret is required"} {
		if !strings.Contains(msg, want) {
			t.Errorf("pogfarm message lacks %q:\n%s", want, msg)
		}
	}

	_, err = LoadClipper(path)
	cProblems := problemsOf(t, err)
	if len(cProblems) != 4 {
		t.Errorf("clipper: want 4 problems, got %d:\n%s", len(cProblems), strings.Join(cProblems, "\n"))
	}
	msg = err.Error()
	for _, want := range []string{"clipper.pogfarm_url", "clipper.api_secret", "too short", "clipper.scratch_dir", "clipper.name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("clipper message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "short\n") || strings.Contains(msg, `"short"`) {
		t.Errorf("validation message must not echo the secret:\n%s", msg)
	}
}

func TestClipperRequiresURLAndDefaultsName(t *testing.T) {
	cleanEnv(t)
	t.Setenv("CLIPPER_API_SECRET", goodSecret)

	_, err := LoadClipper("")
	got := strings.Join(problemsOf(t, err), "\n")
	if !strings.Contains(got, "clipper.pogfarm_url (CLIPPER_POGFARM_URL) is required") {
		t.Errorf("unexpected problems: %s", got)
	}

	t.Setenv("CLIPPER_POGFARM_URL", "http://pogfarm:8080")
	t.Setenv("CLIPPER_NAME", "")
	cfg, err := LoadClipper("")
	if err != nil {
		// A hostname that is not a valid clipper name is reported, not hidden.
		if !strings.Contains(err.Error(), "clipper.name") {
			t.Fatal(err)
		}
		return
	}
	host, _ := os.Hostname()
	if cfg.Name != host {
		t.Errorf("name should default to the hostname %q, got %q", host, cfg.Name)
	}
	if cfg.ScratchDir != "scratch" {
		t.Errorf("scratch_dir default not applied: %q", cfg.ScratchDir)
	}
}

func TestExampleFileLoads(t *testing.T) {
	cleanEnv(t)
	example := filepath.Join("..", "..", "config.example.yaml")
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	t.Setenv("CLIPPER_API_SECRET", goodSecret)
	t.Setenv("CLIPPER_NAME", "example-node")
	t.Setenv("POGFARM_TWITCH_CLIENT_ID", twitchID)
	t.Setenv("POGFARM_TWITCH_CLIENT_SECRET", twitchSecret)

	if _, err := LoadPogfarm(example); err != nil {
		t.Errorf("pogfarm: %v", err)
	}
	if _, err := LoadClipper(example); err != nil {
		t.Errorf("clipper: %v", err)
	}
}

func TestExamplePlaceholderSecretIsRejected(t *testing.T) {
	cleanEnv(t)
	example := filepath.Join("..", "..", "config.example.yaml")
	t.Setenv("CLIPPER_NAME", "example-node")

	_, err := LoadPogfarm(example)
	got := problemsOf(t, err)
	if len(got) != 3 ||
		!strings.Contains(got[0], "pogfarm.api_secret is still the example placeholder") ||
		!strings.Contains(got[1], "pogfarm.twitch.client_id is still the example placeholder") ||
		!strings.Contains(got[2], "pogfarm.twitch.client_secret is still the example placeholder") {
		t.Errorf("pogfarm: unexpected problems: %v", got)
	}

	_, err = LoadClipper(example)
	got = problemsOf(t, err)
	if len(got) != 1 || !strings.Contains(got[0], "clipper.api_secret is still the example placeholder") {
		t.Errorf("clipper: unexpected problems: %v", got)
	}
}

func TestSecretsAreNeverPrinted(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	t.Setenv("CLIPPER_API_SECRET", goodSecret)
	t.Setenv("CLIPPER_POGFARM_URL", "http://pogfarm:8080")
	t.Setenv("CLIPPER_NAME", "node-a")

	p, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}
	c, err := LoadClipper("")
	if err != nil {
		t.Fatal(err)
	}

	outputs := map[string]string{}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		outputs["pogfarm value "+verb] = fmt.Sprintf(verb, *p)
		outputs["pogfarm pointer "+verb] = fmt.Sprintf(verb, p)
		outputs["clipper value "+verb] = fmt.Sprintf(verb, *c)
		outputs["secret "+verb] = fmt.Sprintf(verb, p.APISecret)
		outputs["secret pointer "+verb] = fmt.Sprintf(verb, &p.APISecret)
		outputs["secret in struct "+verb] = fmt.Sprintf(verb, struct{ S Secret }{p.APISecret})
	}
	outputs["pogfarm String"] = p.String()
	outputs["clipper String"] = c.String()
	outputs["secret String"] = p.APISecret.String()
	outputs["print"] = fmt.Sprint(p, c, p.APISecret)

	j, err := json.Marshal(struct {
		P *Pogfarm
		C *Clipper
	}{p, c})
	if err != nil {
		t.Fatal(err)
	}
	outputs["json"] = string(j)

	y, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	outputs["yaml"] = string(y)

	var text, jsonLog bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("loaded", "config", p, "secret", p.APISecret, "clipper", *c)
	slog.New(slog.NewJSONHandler(&jsonLog, nil)).Info("loaded", "config", p, "secret", p.APISecret, "clipper", *c)
	outputs["slog text"] = text.String()
	outputs["slog json"] = jsonLog.String()

	hexSecret := fmt.Sprintf("%x", goodSecret)
	for name, out := range outputs {
		if strings.Contains(out, goodSecret) || strings.Contains(strings.ToLower(out), hexSecret) {
			t.Errorf("%s leaks the secret: %s", name, out)
		}
	}

	if !strings.Contains(p.String(), "api_secret: '[redacted]'") && !strings.Contains(p.String(), `api_secret: "[redacted]"`) {
		t.Errorf("String should show a redacted placeholder, got:\n%s", p.String())
	}
	if !strings.Contains(p.String(), "listen_addr") {
		t.Errorf("String should still show ordinary settings, got:\n%s", p.String())
	}
	if got := (Secret{}).String(); got != "[not set]" {
		t.Errorf("an empty secret should say so, got %q", got)
	}
}

func TestSecretYAMLErrorDoesNotEchoContent(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, "config.yaml", "pogfarm:\n  api_secret: ["+goodSecret+"]\n")
	_, err := LoadPogfarm(path)
	if err == nil {
		t.Fatal("a list is not a valid secret")
	}
	if strings.Contains(err.Error(), goodSecret) {
		t.Errorf("parse error leaks the secret: %v", err)
	}
}

func TestApplyEnvScalarTypes(t *testing.T) {
	type nested struct {
		Retries int `env:"T_RETRIES"`
	}
	type sample struct {
		Name     string        `env:"T_NAME"`
		Enabled  bool          `env:"T_ENABLED"`
		Timeout  time.Duration `env:"T_TIMEOUT"`
		Small    int8          `env:"T_SMALL"`
		Token    Secret        `env:"T_TOKEN"`
		FileOnly string
		Nested   nested
		hidden   string
	}

	t.Run("valid values", func(t *testing.T) {
		t.Setenv("T_NAME", "x")
		t.Setenv("T_ENABLED", "true")
		t.Setenv("T_TIMEOUT", "90s")
		t.Setenv("T_SMALL", "7")
		t.Setenv("T_TOKEN", goodSecret)
		t.Setenv("T_RETRIES", "5")

		s := sample{FileOnly: "keep", hidden: "keep"}
		if problems := applyEnv(&s); len(problems) != 0 {
			t.Fatalf("unexpected problems: %v", problems)
		}
		if s.Name != "x" || !s.Enabled || s.Timeout != 90*time.Second || s.Small != 7 ||
			s.Token.Reveal() != goodSecret || s.Nested.Retries != 5 || s.FileOnly != "keep" || s.hidden != "keep" {
			t.Errorf("unexpected result: %+v", s)
		}
	})

	t.Run("invalid values are all reported and leave fields alone", func(t *testing.T) {
		t.Setenv("T_ENABLED", "maybe")
		t.Setenv("T_TIMEOUT", "soon")
		t.Setenv("T_SMALL", "999")
		t.Setenv("T_RETRIES", "many")

		s := sample{Enabled: true, Timeout: time.Minute}
		problems := applyEnv(&s)
		if len(problems) != 4 {
			t.Fatalf("want 4 problems, got %d: %v", len(problems), problems)
		}
		joined := strings.Join(problems, "\n")
		for _, want := range []string{"T_ENABLED", "T_TIMEOUT", "T_SMALL", "T_RETRIES"} {
			if !strings.Contains(joined, want) {
				t.Errorf("problems lack %s: %s", want, joined)
			}
		}
		if !s.Enabled || s.Timeout != time.Minute {
			t.Errorf("a bad value should not change the field: %+v", s)
		}
	})
}

// Twitch credentials.

const (
	twitchID     = "abcdefghijklmnopqrstuvwxyz0123"
	twitchSecret = "twitch-client-secret-for-tests"
)

func TestTwitchCredentialsAreOptional(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)

	cfg, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Twitch.ClientID != "" || cfg.Twitch.ClientSecret.IsSet() {
		t.Errorf("twitch credentials should be unset by default: %+v", cfg.Twitch)
	}
}

func TestTwitchCredentialsFromFileEnvAndSecretFile(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	path := writeFile(t, "config.yaml", `
pogfarm:
  twitch:
    client_id: file-id
    client_secret: `+otherSecret+`
`)

	cfg, err := LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Twitch.ClientID != "file-id" || cfg.Twitch.ClientSecret.Reveal() != otherSecret {
		t.Errorf("file values not loaded: %+v", cfg.Twitch)
	}

	t.Setenv("POGFARM_TWITCH_CLIENT_ID", twitchID)
	t.Setenv("POGFARM_TWITCH_CLIENT_SECRET", twitchSecret)
	cfg, err = LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Twitch.ClientID != twitchID || cfg.Twitch.ClientSecret.Reveal() != twitchSecret {
		t.Error("the environment should override the file")
	}

	t.Setenv("POGFARM_TWITCH_CLIENT_SECRET", "")
	t.Setenv("POGFARM_TWITCH_CLIENT_SECRET_FILE", writeFile(t, "twitch_secret", twitchSecret+"\n"))
	cfg, err = LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Twitch.ClientSecret.Reveal() != twitchSecret {
		t.Error("secret not read from the secret file")
	}
}

func TestTwitchCredentialsValidation(t *testing.T) {
	tests := []struct {
		name, id, secret string
		want             string
	}{
		{"id without secret", twitchID, "", "pogfarm.twitch.client_secret is required when pogfarm.twitch.client_id is set"},
		{"secret without id", "", twitchSecret, "pogfarm.twitch.client_id is required when pogfarm.twitch.client_secret is set"},
		{"placeholder secret", twitchID, SecretPlaceholder, "pogfarm.twitch.client_secret is still the example placeholder"},
		{"placeholder id", SecretPlaceholder, twitchSecret, "pogfarm.twitch.client_id is still the example placeholder"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("POGFARM_API_SECRET", goodSecret)
			t.Setenv("POGFARM_TWITCH_CLIENT_ID", tt.id)
			t.Setenv("POGFARM_TWITCH_CLIENT_SECRET", tt.secret)

			_, err := LoadPogfarm("")
			got := problemsOf(t, err)
			if len(got) != 1 || !strings.Contains(got[0], tt.want) {
				t.Errorf("want one problem containing %q, got %v", tt.want, got)
			}
			if strings.Contains(err.Error(), twitchSecret) {
				t.Errorf("validation message must not echo the secret: %v", err)
			}
		})
	}
}

func TestTwitchSecretIsNeverPrinted(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	t.Setenv("POGFARM_TWITCH_CLIENT_ID", twitchID)
	t.Setenv("POGFARM_TWITCH_CLIENT_SECRET", twitchSecret)

	p, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}

	outputs := map[string]string{"String": p.String()}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		outputs["pogfarm "+verb] = fmt.Sprintf(verb, *p)
		outputs["twitch "+verb] = fmt.Sprintf(verb, p.Twitch)
	}
	j, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	outputs["json"] = string(j)
	var text bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("loaded", "config", p, "twitch", p.Twitch)
	outputs["slog"] = text.String()

	hexSecret := fmt.Sprintf("%x", twitchSecret)
	for name, out := range outputs {
		if strings.Contains(out, twitchSecret) || strings.Contains(strings.ToLower(out), hexSecret) {
			t.Errorf("%s leaks the twitch client secret: %s", name, out)
		}
	}
	if !strings.Contains(p.String(), "client_id: "+twitchID) {
		t.Errorf("String should still show the client id, got:\n%s", p.String())
	}
}

// Telegram bot.

const goodBotToken = "123456789:AAbbCCddEEffGGhhIIjjKKllMMnnOOppQQ"

func TestTelegramIsOffByDefault(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)

	cfg, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.Enabled() {
		t.Error("the bot must be off when nothing is configured")
	}
}

func TestTelegramFromFile(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, "config.yaml", `
pogfarm:
  api_secret: `+goodSecret+`
  telegram:
    bot_token: "`+goodBotToken+`"
    owner_chat_id: 4242
`)

	cfg, err := LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Telegram.Enabled() || cfg.Telegram.BotToken.Reveal() != goodBotToken || cfg.Telegram.OwnerChatID != 4242 {
		t.Errorf("telegram settings not loaded: enabled=%v owner=%d", cfg.Telegram.Enabled(), cfg.Telegram.OwnerChatID)
	}
}

func TestTelegramFromEnvAndSecretFile(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	path := writeFile(t, "config.yaml", `
pogfarm:
  telegram:
    bot_token: "from-the-file-and-overridden"
    owner_chat_id: 1
`)
	t.Setenv("POGFARM_TELEGRAM_BOT_TOKEN_FILE", writeFile(t, "token", goodBotToken+"\n"))
	t.Setenv("POGFARM_TELEGRAM_OWNER_CHAT_ID", "-1001234567890")

	cfg, err := LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.BotToken.Reveal() != goodBotToken {
		t.Error("token not taken from the secret file")
	}
	if cfg.Telegram.OwnerChatID != -1001234567890 {
		t.Errorf("owner chat id not taken from the environment: %d", cfg.Telegram.OwnerChatID)
	}

	t.Setenv("POGFARM_TELEGRAM_OWNER_CHAT_ID", "me")
	_, err = LoadPogfarm(path)
	if got := strings.Join(problemsOf(t, err), "\n"); !strings.Contains(got, "POGFARM_TELEGRAM_OWNER_CHAT_ID: not a whole number") {
		t.Errorf("unexpected problems: %s", got)
	}
}

func TestTelegramValidation(t *testing.T) {
	cases := []struct {
		name, token, owner, want string
	}{
		{"token without owner", goodBotToken, "", "pogfarm.telegram.owner_chat_id (POGFARM_TELEGRAM_OWNER_CHAT_ID) is required"},
		{"owner without token", "", "4242", "pogfarm.telegram.bot_token is required"},
		{"placeholder token", SecretPlaceholder, "4242", "pogfarm.telegram.bot_token is still the example placeholder"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("POGFARM_API_SECRET", goodSecret)
			t.Setenv("POGFARM_TELEGRAM_BOT_TOKEN", tc.token)
			t.Setenv("POGFARM_TELEGRAM_OWNER_CHAT_ID", tc.owner)

			_, err := LoadPogfarm("")
			got := problemsOf(t, err)
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Errorf("want one problem containing %q, got %v", tc.want, got)
			}
			if tc.token != "" && tc.token != SecretPlaceholder && strings.Contains(err.Error(), tc.token) {
				t.Errorf("validation message leaks the token: %v", err)
			}
		})
	}
}

func TestTelegramTokenIsNeverPrinted(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	t.Setenv("POGFARM_TELEGRAM_BOT_TOKEN", goodBotToken)
	t.Setenv("POGFARM_TELEGRAM_OWNER_CHAT_ID", "4242")

	cfg, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	slog.New(slog.NewTextHandler(&log, nil)).Info("loaded", "config", cfg, "telegram", cfg.Telegram)
	j, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{
		"String": cfg.String(),
		"%+v":    fmt.Sprintf("%+v %+v", cfg, cfg.Telegram),
		"%#v":    fmt.Sprintf("%#v", cfg.Telegram),
		"slog":   log.String(),
		"json":   string(j),
	} {
		if strings.Contains(out, goodBotToken) {
			t.Errorf("%s leaks the bot token: %s", name, out)
		}
	}
	if !strings.Contains(cfg.String(), "owner_chat_id: 4242") {
		t.Errorf("String should still show the owner chat id, got:\n%s", cfg.String())
	}
}

func TestQueueLeaseDuration(t *testing.T) {
	tests := []struct {
		name    string
		file    string // value of pogfarm.queue.lease_duration; "" leaves the key out
		env     string
		want    time.Duration
		problem string
	}{
		{name: "default", want: 2 * time.Minute},
		{name: "from file", file: "90s", want: 90 * time.Second},
		{name: "env overrides file", file: "90s", env: "5m", want: 5 * time.Minute},
		{name: "minimum", env: "10s", want: 10 * time.Second},
		{name: "too short", file: "9s", problem: "pogfarm.queue.lease_duration (POGFARM_QUEUE_LEASE_DURATION) must be at least 10s: got 9s"},
		{name: "zero", env: "0s", problem: "must be at least 10s: got 0s"},
		{name: "negative", file: "-2m", problem: "must be at least 10s: got -2m0s"},
		{name: "not a duration", env: "soon", problem: "POGFARM_QUEUE_LEASE_DURATION: not a duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			content := "pogfarm:\n  api_secret: " + goodSecret + "\n"
			if tt.file != "" {
				content += "  queue:\n    lease_duration: " + tt.file + "\n"
			}
			if tt.env != "" {
				t.Setenv("POGFARM_QUEUE_LEASE_DURATION", tt.env)
			}
			cfg, err := LoadPogfarm(writeFile(t, "config.yaml", content))
			if tt.problem != "" {
				problems := problemsOf(t, err)
				if len(problems) != 1 || !strings.Contains(problems[0], tt.problem) {
					t.Fatalf("problems = %q, want one containing %q", problems, tt.problem)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Queue.LeaseDuration != tt.want {
				t.Errorf("lease duration = %s, want %s", cfg.Queue.LeaseDuration, tt.want)
			}
		})
	}
}

func TestMaxClipBytes(t *testing.T) {
	tests := []struct {
		name    string
		file    string // value of pogfarm.max_clip_bytes; "" leaves the key out
		env     string
		want    int64
		problem string
	}{
		{name: "default", want: DefaultMaxClipBytes},
		{name: "from file", file: "1048576", want: 1 << 20},
		{name: "env overrides file", file: "1048576", env: "2048", want: 2048},
		{name: "zero", file: "0", problem: "pogfarm.max_clip_bytes (POGFARM_MAX_CLIP_BYTES) must be a positive number of bytes: got 0"},
		{name: "negative", env: "-1", problem: "must be a positive number of bytes: got -1"},
		{name: "not a number", env: "lots", problem: "POGFARM_MAX_CLIP_BYTES: not a whole number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			content := "pogfarm:\n  api_secret: " + goodSecret + "\n"
			if tt.file != "" {
				content += "  max_clip_bytes: " + tt.file + "\n"
			}
			if tt.env != "" {
				t.Setenv("POGFARM_MAX_CLIP_BYTES", tt.env)
			}
			cfg, err := LoadPogfarm(writeFile(t, "config.yaml", content))
			if tt.problem != "" {
				problems := problemsOf(t, err)
				if len(problems) != 1 || !strings.Contains(problems[0], tt.problem) {
					t.Fatalf("problems = %q, want one containing %q", problems, tt.problem)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.MaxClipBytes != tt.want {
				t.Errorf("max clip bytes = %d, want %d", cfg.MaxClipBytes, tt.want)
			}
		})
	}
}

func TestTwitchDownloaderPath(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)

	cfg, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TwitchDownloaderPath != "TwitchDownloaderCLI" {
		t.Errorf("default: got %q", cfg.TwitchDownloaderPath)
	}

	path := writeFile(t, "config.yaml", `
pogfarm:
  twitch_downloader_path: /opt/tools/TwitchDownloaderCLI
`)
	if cfg, err = LoadPogfarm(path); err != nil {
		t.Fatal(err)
	}
	if cfg.TwitchDownloaderPath != "/opt/tools/TwitchDownloaderCLI" {
		t.Errorf("from file: got %q", cfg.TwitchDownloaderPath)
	}

	t.Setenv("POGFARM_TWITCH_DOWNLOADER_PATH", "/from/env")
	if cfg, err = LoadPogfarm(path); err != nil {
		t.Fatal(err)
	}
	if cfg.TwitchDownloaderPath != "/from/env" {
		t.Errorf("env should override file: got %q", cfg.TwitchDownloaderPath)
	}

	t.Setenv("POGFARM_TWITCH_DOWNLOADER_PATH", "")
	path = writeFile(t, "config.yaml", `
pogfarm:
  twitch_downloader_path: ""
`)
	_, err = LoadPogfarm(path)
	got := problemsOf(t, err)
	if len(got) != 1 || !strings.Contains(got[0], "pogfarm.twitch_downloader_path (POGFARM_TWITCH_DOWNLOADER_PATH) must not be empty") {
		t.Errorf("empty path: got problems %q", got)
	}
}
