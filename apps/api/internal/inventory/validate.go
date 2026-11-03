package inventory

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SlugPattern is what an environment's slug must match: the environment
// as the collectors and the telemetry name it, each collector's
// `environment`, the `environment` metric label and the logs'
// deployment.environment.name (ADR 0017 §2). Lower-case letters, digits
// and hyphens, starting and ending with a letter or a digit, at most 63:
// a valid label value, URL segment and Kubernetes name alike. The spec's
// Slug schema has the same pattern, and a test keeps them equal.
const SlugPattern = `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`

// IdentifierPattern is what an application's platform and a component's
// type must match, so that code can compare them: lower-case letters,
// digits and underscores, starting with a letter, at most 63.
const IdentifierPattern = `^[a-z][a-z0-9_]{0,62}$`

var (
	slugRE       = regexp.MustCompile(SlugPattern)
	identifierRE = regexp.MustCompile(IdentifierPattern)
)

// Limits on what the fields hold, as the spec states them.
const (
	maxNameRunes        = 200
	maxDescriptionRunes = 2000
	maxTextRunes        = 200
	maxLabels           = 64
	maxLabelKeyRunes    = 63
	maxLabelValueRunes  = 256
)

// The values the tables' check constraints allow.
var (
	statuses         = []string{StatusActive, StatusDisabled}
	environmentTypes = []string{"production", "staging", "development", "test"}
	criticalities    = []string{"low", "medium", "high", "critical"}
)

func invalid(field, format string, args ...any) error {
	return &InvalidError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// name returns v trimmed, or an error unless it then has 1 to 200
// characters and no control characters.
func name(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", invalid(field, "required")
	}
	return text(field, v, maxNameRunes, false)
}

// text returns v trimmed, or an error unless it then has at most max
// characters and no control characters; multiline allows line breaks and
// tabs.
func text(field, v string, max int, multiline bool) (string, error) {
	v = strings.TrimSpace(v)
	if !utf8.ValidString(v) {
		return "", invalid(field, "not valid UTF-8")
	}
	if n := utf8.RuneCountInString(v); n > max {
		return "", invalid(field, "%d characters; want at most %d", n, max)
	}
	if strings.ContainsFunc(v, func(r rune) bool {
		return unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\r' || r == '\t'))
	}) {
		return "", invalid(field, "holds control characters")
	}
	return v, nil
}

func slug(v string) (string, error) {
	if !slugRE.MatchString(v) {
		return "", invalid("slug", "%q: want lower-case letters, digits and hyphens, 1 to 63, starting and ending with a letter or a digit, such as prod", v)
	}
	return v, nil
}

func identifier(field, v string) (string, error) {
	if !identifierRE.MatchString(v) {
		return "", invalid(field, "%q: want lower-case letters, digits and underscores, starting with a letter, at most 63", v)
	}
	return v, nil
}

// oneOf returns v, or def when v is empty, or an error unless it is one of
// allowed.
func oneOf(field, v, def string, allowed []string) (string, error) {
	if v == "" {
		v = def
	}
	if !slices.Contains(allowed, v) {
		return "", invalid(field, "%q: want one of %s", v, strings.Join(allowed, ", "))
	}
	return v, nil
}

// labels returns a copy of m, never nil, or an error unless it has at most
// 64 entries, each key 1 to 63 characters and each value at most 256,
// none with control characters.
func labels(m map[string]string) (map[string]string, error) {
	if len(m) > maxLabels {
		return nil, invalid("labels", "%d labels; want at most %d", len(m), maxLabels)
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if k == "" || !utf8.ValidString(k) || utf8.RuneCountInString(k) > maxLabelKeyRunes || strings.ContainsFunc(k, unicode.IsControl) {
			return nil, invalid("labels", "key %q: want 1 to %d characters, without control characters", k, maxLabelKeyRunes)
		}
		v := m[k]
		if !utf8.ValidString(v) || utf8.RuneCountInString(v) > maxLabelValueRunes || strings.ContainsFunc(v, unicode.IsControl) {
			return nil, invalid("labels", "value of %q: want at most %d characters, without control characters", k, maxLabelValueRunes)
		}
	}
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out, nil
}
