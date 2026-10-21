package collect

import (
	"time"

	"github.com/glueops/inventory-agent/internal/logging"
)

// FormatTime normalises a time to RFC 3339 UTC with millisecond precision
// and a Z suffix, the payload's single timestamp format.
func FormatTime(t time.Time) string {
	return t.UTC().Format(logging.TimeLayout)
}

// formatTimePtr returns nil for a zero time, otherwise the formatted time.
func formatTimePtr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := FormatTime(t)
	return &s
}

// parseHelmTime parses the timestamp strings Helm writes into release JSON
// (RFC 3339 with variable fractional precision). An empty or unparseable
// string yields the zero time.
func parseHelmTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
