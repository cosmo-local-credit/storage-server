// Package config holds the rules for reading this service's configuration from
// the environment. It lives outside cmd/ so those rules are testable: package
// main calls flag.Parse in its init, which a test binary cannot survive.
package config

import "strings"

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
