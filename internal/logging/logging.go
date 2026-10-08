// Package logging builds the structured JSON logger used by the agent.
//
// Every log line is a single JSON object with an RFC 3339 UTC millisecond
// timestamp. Fixed reason codes are attached under the "reason" key so Loki
// queries can match on them.
package logging

import (
	"io"
	"log/slog"
	"strings"
	"time"
)

// Reason codes used in log lines. Section error codes live in package schema.
const (
	ReasonSendFailed            = "send_failed"
	ReasonNoEndpointConfigured  = "no_endpoint_configured"
	ReasonInvalidIngestURL      = "invalid_ingest_url"
	ReasonInvalidConfig         = "invalid_config"
	ReasonPayloadTruncated      = "payload_truncated"
	ReasonClusterUIDUnavailable = "cluster_uid_unavailable"
	ReasonKubeClientUnavailable = "kube_client_unavailable"
	ReasonInternalError         = "internal_error"
)

// TimeLayout is the RFC 3339 UTC millisecond layout used everywhere.
const TimeLayout = "2006-01-02T15:04:05.000Z"

// New returns a JSON slog.Logger writing to w at the given level
// (debug|info|warn|error; anything else means info).
func New(level string, w io.Writer) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: ParseLevel(level),
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				if t, ok := a.Value.Any().(time.Time); ok {
					return slog.String(slog.TimeKey, t.UTC().Format(TimeLayout))
				}
			}
			return a
		},
	})
	return slog.New(h)
}

// ParseLevel maps a string to a slog.Level, defaulting to Info.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
