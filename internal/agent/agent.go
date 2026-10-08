// Package agent orchestrates one run: validate the endpoint, collect the
// envelope, pack it, send it, and emit exactly one summary log line. A run
// always ends with exit code 0.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/glueops/inventory-agent/internal/collect"
	"github.com/glueops/inventory-agent/internal/config"
	"github.com/glueops/inventory-agent/internal/logging"
	"github.com/glueops/inventory-agent/internal/schema"
	"github.com/glueops/inventory-agent/internal/send"
)

// Overall run statuses reported in the summary line.
const (
	StatusOK                   = "ok"                     // sent, every section ok
	StatusPartial              = "partial"                // sent, at least one section errored
	StatusSendFailed           = logging.ReasonSendFailed // collected but not accepted by the receiver
	StatusNoEndpointConfigured = logging.ReasonNoEndpointConfigured
	StatusInvalidIngestURL     = logging.ReasonInvalidIngestURL
	StatusInvalidConfig        = logging.ReasonInvalidConfig // emitted by main before Run
	StatusInternalError        = logging.ReasonInternalError
)

// Sender is the minimal interface the agent needs from package send.
type Sender interface {
	Send(ctx context.Context, gzBody []byte) send.Result
}

// Deps are the injectable collaborators of a run.
type Deps struct {
	// Client may be nil; every section then reports api_unavailable.
	Client kubernetes.Interface
	// Sender posts the payload. nil means build one from the config.
	Sender Sender
	// Now supplies the clock (tests). nil means time.Now.
	Now func() time.Time
	// Version is the collector version (image tag).
	Version string
}

// Summary is what the final log line reports; returned for tests.
type Summary struct {
	RunID  string
	Status string
	// Reason is the fixed reason code for a non-ok run, empty when ok.
	Reason string
	// Error is a short, log-safe description of what went wrong.
	Error      string
	BytesSent  int
	HTTPStatus int
	Attempts   int
	Sections   map[string]string
	PodRows    int
	Truncated  bool
	// HelmDecodeFailed counts Helm release Secrets skipped as undecodable.
	HelmDecodeFailed int
	Envelope         *schema.Envelope
}

// Run executes one collection run and returns the process exit code, which
// is always 0, together with the summary.
func Run(ctx context.Context, cfg config.Config, deps Deps, log *slog.Logger) (exitCode int, summary Summary) {
	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	start := now()
	summary = Summary{RunID: NewRunID(start), Sections: map[string]string{}}
	summaryLog := log
	log = log.With("run_id", summary.RunID)

	defer func() {
		if r := recover(); r != nil {
			summary.Status = StatusInternalError
			summary.Reason = logging.ReasonInternalError
			summary.Error = "panic: " + fmt.Sprint(r)
			log.Error("run panicked", "reason", logging.ReasonInternalError, "panic", fmt.Sprint(r))
		}
		LogSummary(summaryLog, summary)
		exitCode = 0
	}()

	// Endpoint checks come first: with no usable endpoint there is nothing
	// to collect for.
	if cfg.IngestURL == "" {
		summary.Status = StatusNoEndpointConfigured
		summary.Reason = logging.ReasonNoEndpointConfigured
		log.Warn("no ingest endpoint configured, nothing to send", "reason", logging.ReasonNoEndpointConfigured)
		return 0, summary
	}
	if err := config.ValidateIngestURL(cfg.IngestURL, cfg.DevMode); err != nil {
		summary.Status = StatusInvalidIngestURL
		summary.Reason = logging.ReasonInvalidIngestURL
		summary.Error = err.Error()
		log.Error("ingest url rejected", "reason", logging.ReasonInvalidIngestURL, "error", err.Error())
		return 0, summary
	}

	env, stats := collect.Envelope(ctx, deps.Client, collect.Params{
		CaptainDomain:        cfg.CaptainDomain,
		PlatformChartVersion: cfg.PlatformChartVersion,
		CollectorVersion:     deps.Version,
		RunID:                summary.RunID,
		HelmNamespace:        cfg.HelmNamespace,
		PodNamespaces:        cfg.PodNamespaces,
		MaxPodRows:           cfg.MaxPodRows,
	}, start, log)

	body, dropped, err := send.Pack(&env, cfg.MaxGzipBytes)
	if err != nil {
		// Marshalling closed structs cannot realistically fail; treat it as
		// an internal error and stop.
		summary.Status = StatusInternalError
		summary.Reason = logging.ReasonInternalError
		summary.Error = err.Error()
		log.Error("pack failed", "reason", logging.ReasonInternalError, "error", err.Error())
		return 0, summary
	}
	if dropped > 0 {
		log.Warn("pod_images truncated to fit size cap", "reason", logging.ReasonPayloadTruncated,
			"dropped_rows", dropped, "max_gzip_bytes", cfg.MaxGzipBytes, "gzip_bytes", len(body))
	}
	if cfg.MaxGzipBytes > 0 && len(body) > cfg.MaxGzipBytes {
		log.Warn("payload exceeds size cap even with no pod_images rows; sending anyway",
			"reason", logging.ReasonPayloadTruncated, "gzip_bytes", len(body), "max_gzip_bytes", cfg.MaxGzipBytes)
	}

	summary.Envelope = &env
	summary.Sections = sectionStatuses(&env)
	summary.PodRows = len(env.Datasets.PodImages.Data)
	summary.Truncated = env.Datasets.PodImages.Truncated
	summary.HelmDecodeFailed = stats.HelmDecodeFailed

	sender := deps.Sender
	if sender == nil {
		sender = send.New(send.Options{
			URL:       cfg.IngestURL,
			Timeout:   cfg.HTTPTimeout,
			Retries:   cfg.Retries,
			UserAgent: "inventory-agent/" + deps.Version,
		})
	}
	res := sender.Send(ctx, body)
	summary.HTTPStatus = res.HTTPStatus
	summary.Attempts = res.Attempts
	if !res.OK() {
		summary.Status = StatusSendFailed
		summary.Reason = logging.ReasonSendFailed
		summary.Error = errString(res.Err)
		log.Error("snapshot not accepted by receiver", "reason", logging.ReasonSendFailed,
			"http_status", res.HTTPStatus, "attempts", res.Attempts, "error", errString(res.Err))
		return 0, summary
	}
	summary.BytesSent = len(body)
	summary.Status = StatusOK
	for _, s := range summary.Sections {
		if s != schema.StatusOK {
			summary.Status = StatusPartial
			break
		}
	}
	return 0, summary
}

// NewRunID returns "<yyyymmddHHMMSS>_<8 hex>" in UTC.
func NewRunID(t time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is essentially impossible; fall back to the
		// clock so the ID is still unique enough.
		n := uint32(t.UnixNano())
		b = [4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
	return t.UTC().Format("20060102150405") + "_" + hex.EncodeToString(b[:])
}

func sectionStatuses(env *schema.Envelope) map[string]string {
	status := func(st, code string) string {
		if st == schema.StatusOK {
			return st
		}
		return st + ":" + code
	}
	return map[string]string{
		"cluster":       status(env.Datasets.Cluster.Status, env.Datasets.Cluster.Error),
		"helm_releases": status(env.Datasets.HelmReleases.Status, env.Datasets.HelmReleases.Error),
		"pod_images":    status(env.Datasets.PodImages.Status, env.Datasets.PodImages.Error),
	}
}

// LogSummary emits the single end-of-run summary line. main calls it
// directly for runs that end before Run starts (invalid configuration).
func LogSummary(log *slog.Logger, s Summary) {
	attrs := []any{
		"run_id", s.RunID,
		"status", s.Status,
		"bytes_sent", s.BytesSent,
		"http_status", s.HTTPStatus,
		"attempts", s.Attempts,
		"section_cluster", s.Sections["cluster"],
		"section_helm_releases", s.Sections["helm_releases"],
		"section_pod_images", s.Sections["pod_images"],
		"pod_rows", s.PodRows,
		"truncated", s.Truncated,
		"helm_decode_failed", s.HelmDecodeFailed,
	}
	if s.Reason != "" {
		attrs = append(attrs, "reason", s.Reason)
	}
	if s.Error != "" {
		attrs = append(attrs, "error", s.Error)
	}
	if s.Status == StatusOK {
		log.Info("run summary", attrs...)
	} else {
		log.Warn("run summary", attrs...)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
