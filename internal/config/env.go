// Package config holds the rules for reading this service's configuration from
// the environment. It lives outside cmd/ so those rules are testable: package
// main calls flag.Parse in its init, which a test binary cannot survive.
package config

import (
	"strings"

	"github.com/knadh/koanf/v2"
)

// EnvPrefix is the prefix every configuration variable carries.
const EnvPrefix = "STORAGE_"

// EnvOverride maps one STORAGE_* variable onto a config key and value.
//
// The same rules clc-core applies to its CORE_* variables, so a service
// configured entirely from a compose env_file behaves the same in both repos and
// no config file has to be mounted alongside the image.
//
// Two conversions matter, and their order does:
//
//   - a docker env_file passes "\n" as two literal bytes, which pem.Decode
//     rejects, so escaped newlines become real ones. auth.public_key is
//     unusable from the environment without this;
//   - api.origin, api.allowed_folders and image.allowed_widths are lists, and a
//     plain string override would land as a single element — one bogus origin,
//     or one unparseable width.
//
// Unescaping runs first so a PEM block is recognised as multiline and returned
// whole. Splitting first would cut "-----BEGIN PUBLIC KEY-----" at its spaces.
//
// A split list is []any rather than []string because that is the only shape both
// of koanf's typed list getters accept: Ints matches []int, []int64 and
// []interface{} — never []string — so image.allowed_widths from a []string would
// read back empty and MustInts would panic at startup.
func EnvOverride(key, value string) (string, any) {
	mappedKey := strings.ReplaceAll(strings.ToLower(
		strings.TrimPrefix(key, EnvPrefix)), "__", ".")

	value = strings.ReplaceAll(value, "\\n", "\n")

	switch {
	case strings.Contains(value, "\n"):
		return mappedKey, value
	case strings.Contains(value, ","):
		return mappedKey, splitList(value, ",")
	case strings.Contains(value, " "):
		return mappedKey, splitList(value, " ")
	default:
		return mappedKey, value
	}
}

func splitList(value, sep string) []any {
	parts := strings.Split(value, sep)
	list := make([]any, 0, len(parts))
	for _, part := range parts {
		list = append(list, part)
	}
	return list
}

// Strings reads a list key that may legitimately be empty or hold one entry.
//
// EnvOverride leaves a single value scalar, and koanf's Strings answers a scalar
// with an empty list, so one host set from the environment would silently read
// as none. Blank entries are dropped, so an empty variable means an empty list
// rather than one empty-string entry.
func Strings(ko *koanf.Koanf, path string) []string {
	var raw []string
	if s, ok := ko.Get(path).(string); ok {
		raw = strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	} else {
		raw = ko.Strings(path)
	}

	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
