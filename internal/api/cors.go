package api

import (
	"strings"

	"github.com/labstack/echo/v5"
)

// newOriginMatcher matches an Origin header against the configured allowlist,
// where a single `*` stands for exactly one DNS label.
//
// Echo's default matcher is a literal, case-insensitive string compare, so
// `https://*.clc-app.pages.dev` would never match a branch preview on its own.
// The label is required to be non-empty and to contain no dot, so one entry
// covers `https://feature-x.clc-app.pages.dev` without also admitting
// `https://a.b.clc-app.pages.dev` or the bare apex — list the apex separately if
// it needs to be allowed. The semantics deliberately mirror clc-core's
// util.OriginAllowed, so an origin added there behaves the same here.
func newOriginMatcher(allowed []string) func(*echo.Context, string) (string, bool, error) {
	patterns := append([]string(nil), allowed...)

	return func(_ *echo.Context, origin string) (string, bool, error) {
		if origin == "" {
			return "", false, nil
		}
		for _, pattern := range patterns {
			if originMatches(pattern, origin) {
				// The caller's own origin, never `*`: the response may carry
				// credentials, and a wildcard is invalid in that case.
				return origin, true, nil
			}
		}
		return "", false, nil
	}
}

func originMatches(pattern, origin string) bool {
	star := strings.IndexByte(pattern, '*')
	if star < 0 {
		return strings.EqualFold(pattern, origin)
	}

	prefix, suffix := pattern[:star], pattern[star+1:]
	if len(origin) <= len(prefix)+len(suffix) {
		return false
	}
	if !strings.EqualFold(prefix, origin[:len(prefix)]) {
		return false
	}
	if !strings.EqualFold(suffix, origin[len(origin)-len(suffix):]) {
		return false
	}

	label := origin[len(prefix) : len(origin)-len(suffix)]
	return label != "" && !strings.Contains(label, ".")
}
