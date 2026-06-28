package metrics

import "unicode/utf8"

// TruncateLabelValue truncates s to at most maxBytes bytes, walking back to a
// UTF-8 rune boundary so the result stays valid UTF-8. Prometheus label values
// must be valid UTF-8 (MustNewConstMetric panics otherwise); bounding the byte
// length also caps cardinality from user-controlled Plex strings.
func TruncateLabelValue(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	i := maxBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}
