// Package send POSTs the gzipped envelope to the ingest endpoint with a
// bounded timeout and a few quick retries.
package send

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Options configure a Sender.
type Options struct {
	// URL is the ingest endpoint. Scheme validation happens in config.
	URL string
	// Timeout bounds each individual attempt.
	Timeout time.Duration
	// Retries is the number of extra attempts after the first one.
	Retries int
	// Backoff is the base delay between attempts (multiplied by attempt
	// number). Zero means the default of 500ms.
	Backoff time.Duration
	// UserAgent is sent as the User-Agent header.
	UserAgent string
	// BearerToken, when non-empty, is sent as "Authorization: Bearer ...".
	// v1 never sets it (no authentication yet); this is the reserved slot so
	// adding a token later is a config change, not a code or schema change.
	BearerToken string
	// Transport overrides the HTTP transport (tests). nil means default.
	Transport http.RoundTripper
}

// Result describes the outcome of a Send.
type Result struct {
	// HTTPStatus is the status code of the last response, 0 if none arrived.
	HTTPStatus int
	// Attempts is how many requests were made.
	Attempts int
	// Err is nil on a 2xx response.
	Err error
}

// OK reports whether the snapshot was accepted (2xx).
func (r Result) OK() bool { return r.Err == nil }

// Sender posts payloads.
type Sender struct {
	opts   Options
	client *http.Client
}

// New builds a Sender.
func New(opts Options) *Sender {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Backoff <= 0 {
		opts.Backoff = 500 * time.Millisecond
	}
	if opts.Retries < 0 {
		opts.Retries = 0
	}
	return &Sender{
		opts: opts,
		client: &http.Client{
			Timeout:   opts.Timeout,
			Transport: opts.Transport,
			// Never follow redirects: a 301/302 from the ingress would turn
			// the POST into a body-less GET (and could downgrade https to
			// http). The 3xx is surfaced as a final StatusError instead.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Send POSTs gzBody (already gzipped JSON) with Content-Type
// application/json and Content-Encoding gzip. It retries on network errors,
// timeouts, 429 and 5xx; any other non-2xx status (including every 3xx,
// which is never followed) is final.
func (s *Sender) Send(ctx context.Context, gzBody []byte) Result {
	var res Result
	for attempt := 1; attempt <= 1+s.opts.Retries; attempt++ {
		res.Attempts = attempt
		status, err := s.post(ctx, gzBody)
		res.HTTPStatus = status
		res.Err = err
		if err == nil {
			return res
		}
		if !retryable(status, err) || attempt > s.opts.Retries || ctx.Err() != nil {
			return res
		}
		select {
		case <-ctx.Done():
			res.Err = ctx.Err()
			return res
		case <-time.After(s.opts.Backoff * time.Duration(attempt)):
		}
	}
	return res
}

func (s *Sender) post(ctx context.Context, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.opts.URL, bytes.NewReader(body))
	if err != nil {
		// The parse error would echo the URL; the URL was validated by
		// config, so only say that building the request failed.
		return 0, errors.New("building request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	if s.opts.UserAgent != "" {
		req.Header.Set("User-Agent", s.opts.UserAgent)
	}
	if s.opts.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.opts.BearerToken)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, sanitize(err)
	}
	defer resp.Body.Close()
	// Drain (bounded) so the connection can be reused; the body is not used.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, &StatusError{Code: resp.StatusCode}
}

// StatusError is returned for non-2xx responses.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("unexpected HTTP status %d", e.Code) }

// TransportError is a transport-level failure with the request URL reduced
// to its host. *url.Error's text embeds the full URL (path, query string,
// userinfo), which must never reach the logs; only Op, host and the inner
// error are kept.
type TransportError struct {
	Op   string
	Host string
	Err  error
}

func (e *TransportError) Error() string { return fmt.Sprintf("%s %s: %v", e.Op, e.Host, e.Err) }

func (e *TransportError) Unwrap() error { return e.Err }

// Timeout reports whether the inner error is a timeout (net.Error).
func (e *TransportError) Timeout() bool {
	var ne net.Error
	return errors.As(e.Err, &ne) && ne.Timeout()
}

func sanitize(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	host := ""
	if u, perr := url.Parse(ue.URL); perr == nil {
		host = u.Host
	}
	return &TransportError{Op: ue.Op, Host: host, Err: ue.Err}
}

func retryable(status int, err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	// Any transport-level error (connection refused, reset, timeout).
	return err != nil && status == 0
}
