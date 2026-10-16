// Package threading implements RFC 5322 header normalization and conversation thread linking.
package threading

import (
	"regexp"
	"strings"
)

var (
	replyPrefixRe = regexp.MustCompile(`(?i)^(?:\s*(?:re|fw|fwd|aw|wg|sv|antw)(?:\[\d+\])?:\s*)+`)
	whitespaceRe  = regexp.MustCompile(`\s+`)
	angleIDRe     = regexp.MustCompile(`<([^<>]+)>`)
)

// NormalizeSubject strips leading reply/forward prefixes (Re:, Fwd:, AW:, etc.) and collapses whitespace.
// It also returns true if at least one reply/forward prefix was present.
func NormalizeSubject(raw string) (normalized string, hadPrefix bool) {
	trimmed := strings.TrimSpace(raw)
	loc := replyPrefixRe.FindStringIndex(trimmed)
	if loc != nil && loc[0] == 0 {
		hadPrefix = true
		trimmed = strings.TrimSpace(trimmed[loc[1]:])
	}
	normalized = whitespaceRe.ReplaceAllString(trimmed, " ")
	return normalized, hadPrefix
}

// NormalizeMessageID strips surrounding angle brackets and whitespace from a single Message-ID.
func NormalizeMessageID(raw string) string {
	s := strings.TrimSpace(raw)
	if matches := angleIDRe.FindStringSubmatch(s); len(matches) == 2 {
		return strings.TrimSpace(matches[1])
	}
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return strings.TrimSpace(s)
}

// ParseMessageIDList extracts all normalized Message-IDs from an In-Reply-To or References header.
func ParseMessageIDList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	matches := angleIDRe.FindAllStringSubmatch(raw, -1)
	if len(matches) > 0 {
		out := make([]string, 0, len(matches))
		seen := make(map[string]struct{}, len(matches))
		for _, m := range matches {
			id := strings.TrimSpace(m[1])
			if id == "" {
				continue
			}
			if _, exists := seen[id]; !exists {
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
		return out
	}

	fields := strings.Fields(raw)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if id := NormalizeMessageID(f); id != "" {
			out = append(out, id)
		}
	}
	return out
}
