package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"

	"go.yaml.in/yaml/v3"
)

const (
	redacted = "[redacted]"
	notSet   = "[not set]"
)

// Secret holds a sensitive value such as a token or shared key.
//
// Every way of printing, logging or encoding a Secret yields a placeholder
// instead of the value. The only way to get the value is Reveal, so a search
// for Reveal finds every place a secret leaves this type.
type Secret struct {
	value string
}

// NewSecret wraps a sensitive value.
func NewSecret(value string) Secret {
	return Secret{value: value}
}

// Reveal returns the actual value. Never pass the result to a logger.
func (s Secret) Reveal() string {
	return s.value
}

// IsSet reports whether a value is present.
func (s Secret) IsSet() bool {
	return s.value != ""
}

// String returns a placeholder that says only whether a value is present.
func (s Secret) String() string {
	if s.value == "" {
		return notSet
	}
	return redacted
}

// Format makes every fmt verb, including %#v and %x, print the placeholder.
func (s Secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, s.String())
}

// GoString covers callers that ask for the Go-syntax form directly.
func (s Secret) GoString() string {
	return s.String()
}

// MarshalText keeps the value out of JSON and any other text encoding.
func (s Secret) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

// MarshalYAML keeps the value out of YAML output.
func (s Secret) MarshalYAML() (any, error) {
	return s.String(), nil
}

// LogValue keeps the value out of structured logs.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue(s.String())
}

// UnmarshalYAML reads the value from a config file. The error never includes
// the offending content, since it may be part of a secret.
func (s *Secret) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return errors.New("secret must be a plain string")
	}
	if node.Tag == "!!null" {
		s.value = ""
		return nil
	}
	s.value = node.Value
	return nil
}
