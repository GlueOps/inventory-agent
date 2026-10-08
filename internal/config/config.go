// Package config parses and validates the agent's environment configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Defaults.
const (
	DefaultHelmNamespace = "glueops-core"
	DefaultPodNamespaces = "kube-system,glueops-core"
	DefaultMaxPodRows    = 5000
	DefaultMaxGzipBytes  = 2 * 1024 * 1024
	DefaultHTTPTimeout   = 10 * time.Second
	DefaultRetries       = 2
	DefaultLogLevel      = "info"
)

// Config is the fully parsed agent configuration.
type Config struct {
	// CaptainDomain is required and is the join key for the cluster.
	CaptainDomain string
	// IngestURL is where the snapshot is POSTed. Empty means "collect
	// nothing, log no_endpoint_configured, exit 0".
	IngestURL string
	// PodNamespaces are the namespaces to list pods in, in order.
	PodNamespaces []string
	// HelmNamespace is where Helm release Secrets are read.
	HelmNamespace string
	// PlatformChartVersion is the declared glueops-platform chart version.
	PlatformChartVersion string
	// DevMode relaxes the https-only rule for INGEST_URL.
	DevMode bool
	// LogLevel is debug|info|warn|error.
	LogLevel string
	// MaxPodRows caps the pod_images rows before truncation.
	MaxPodRows int
	// MaxGzipBytes caps the gzipped payload before truncation.
	MaxGzipBytes int
	// HTTPTimeout is the per-request timeout for the ingest POST.
	HTTPTimeout time.Duration
	// Retries is the number of additional attempts after the first POST.
	Retries int
}

// Load reads configuration through getenv (usually os.Getenv) and validates
// it. The returned error is suitable for logging; it never contains values
// other than the offending variable name and a short reason.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	get := func(key, def string) string {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return def
		}
		return v
	}

	cfg := Config{
		CaptainDomain:        get("CAPTAIN_DOMAIN", ""),
		IngestURL:            get("INGEST_URL", ""),
		HelmNamespace:        get("HELM_NAMESPACE", DefaultHelmNamespace),
		PlatformChartVersion: get("GLUEOPS_PLATFORM_CHART_VERSION", ""),
		LogLevel:             get("LOG_LEVEL", DefaultLogLevel),
	}

	switch {
	case cfg.CaptainDomain == "":
		errs = append(errs, errors.New("CAPTAIN_DOMAIN is required"))
	case strings.Contains(strings.ToLower(cfg.CaptainDomain), "placeholder"):
		errs = append(errs, errors.New("CAPTAIN_DOMAIN still contains \"placeholder\""))
	}

	cfg.PodNamespaces = splitList(get("POD_NAMESPACES", DefaultPodNamespaces))

	var err error
	if cfg.DevMode, err = parseBool(get("DEV_MODE", "false")); err != nil {
		errs = append(errs, fmt.Errorf("DEV_MODE: %w", err))
	}
	if cfg.MaxPodRows, err = parsePositiveInt(get("MAX_POD_ROWS", strconv.Itoa(DefaultMaxPodRows))); err != nil {
		errs = append(errs, fmt.Errorf("MAX_POD_ROWS: %w", err))
	}
	if cfg.MaxGzipBytes, err = parsePositiveInt(get("MAX_GZIP_BYTES", strconv.Itoa(DefaultMaxGzipBytes))); err != nil {
		errs = append(errs, fmt.Errorf("MAX_GZIP_BYTES: %w", err))
	}
	if cfg.HTTPTimeout, err = time.ParseDuration(get("HTTP_TIMEOUT", DefaultHTTPTimeout.String())); err != nil || cfg.HTTPTimeout <= 0 {
		errs = append(errs, errors.New("HTTP_TIMEOUT: must be a positive duration such as 10s"))
	}
	if cfg.Retries, err = strconv.Atoi(get("RETRIES", strconv.Itoa(DefaultRetries))); err != nil || cfg.Retries < 0 || cfg.Retries > 10 {
		errs = append(errs, errors.New("RETRIES: must be an integer between 0 and 10"))
	}

	return cfg, errors.Join(errs...)
}

// ValidateIngestURL checks the URL scheme rule: https is required unless
// devMode is set, in which case http is also accepted. An empty URL is not
// an error here; callers treat it as "no endpoint configured".
func ValidateIngestURL(raw string, devMode bool) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("INGEST_URL is not a valid URL")
	}
	if u.Host == "" {
		return errors.New("INGEST_URL has no host")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if devMode {
			return nil
		}
		return errors.New("INGEST_URL must use https:// (http is only allowed with DEV_MODE=true)")
	default:
		return fmt.Errorf("INGEST_URL has unsupported scheme %q", u.Scheme)
	}
}

func splitList(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "1", "t", "true", "yes", "y", "on":
		return true, nil
	case "0", "f", "false", "no", "n", "off":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean %q", s)
}

func parsePositiveInt(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, errors.New("must be a positive integer")
	}
	return n, nil
}
