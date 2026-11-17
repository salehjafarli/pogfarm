package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// resolvePath picks the config file: the explicit path first, then
// POGFARM_CONFIG. An empty result means no file is read.
func resolvePath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return os.Getenv(EnvConfigPath)
}

// readFile decodes the config file over the defaults already in f. Keys that
// are absent keep their defaults; keys that are not recognised are an error,
// so a typo cannot silently fall back to a default.
func readFile(path string, f *file) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: read file: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(f); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return nil
}

var (
	secretType   = reflect.TypeFor[Secret]()
	durationType = reflect.TypeFor[time.Duration]()
)

// applyEnv overrides fields of the struct that cfg points to from the
// environment variables named in their env tags, and returns a description of
// every value that could not be used. An empty variable counts as unset.
//
// Secret fields can also be supplied through <env>_FILE, a file whose contents
// are the value; this is how container secrets are mounted. Nested structs are
// followed. Fields without an env tag are file-only.
func applyEnv(cfg any) []string {
	return applyEnvValue(reflect.ValueOf(cfg).Elem())
}

func applyEnvValue(v reflect.Value) []string {
	var problems []string
	t := v.Type()
	for i := range t.NumField() {
		field := v.Field(i)
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		if sf.Type.Kind() == reflect.Struct && sf.Type != secretType {
			problems = append(problems, applyEnvValue(field)...)
			continue
		}
		name := sf.Tag.Get("env")
		if name == "" {
			continue
		}
		if sf.Type == secretType {
			value, problem := secretFromEnv(name)
			if problem != "" {
				problems = append(problems, problem)
			} else if value != "" {
				field.Set(reflect.ValueOf(NewSecret(value)))
			}
			continue
		}
		raw := os.Getenv(name)
		if raw == "" {
			continue
		}
		if err := setScalar(field, raw); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
		}
	}
	return problems
}

// secretFromEnv reads a secret from name or from the file named by name_FILE.
// It returns an empty value when neither is set. Messages never include the
// secret itself.
func secretFromEnv(name string) (value, problem string) {
	direct := os.Getenv(name)
	fileVar := name + "_FILE"
	path := os.Getenv(fileVar)
	switch {
	case direct != "" && path != "":
		return "", fmt.Sprintf("%s and %s are both set; use only one", name, fileVar)
	case direct != "":
		return direct, ""
	case path == "":
		return "", ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Sprintf("%s: cannot read secret file: %v", fileVar, err)
	}
	// Secret files usually end with a newline that is not part of the value.
	value = strings.TrimRight(string(data), "\r\n")
	if value == "" {
		return "", fmt.Sprintf("%s: secret file %s is empty", fileVar, path)
	}
	return value, ""
}

// setScalar parses raw into a string, bool, integer or duration field.
func setScalar(field reflect.Value, raw string) error {
	switch {
	case field.Type() == durationType:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("not a duration such as \"30s\" or \"5m\": %q", raw)
		}
		field.SetInt(int64(d))
	case field.Kind() == reflect.String:
		field.SetString(raw)
	case field.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("not true or false: %q", raw)
		}
		field.SetBool(b)
	case field.CanInt():
		n, err := strconv.ParseInt(raw, 10, field.Type().Bits())
		if err != nil {
			return fmt.Errorf("not a whole number: %q", raw)
		}
		field.SetInt(n)
	default:
		return fmt.Errorf("fields of type %s cannot be set from the environment", field.Type())
	}
	return nil
}
