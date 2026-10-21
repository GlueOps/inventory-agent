package send

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glueops/inventory-agent/internal/schema"
)

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type capture struct {
	headers http.Header
	body    []byte
}

func newSender(url string, retries int) *Sender {
	return New(Options{URL: url, Timeout: 2 * time.Second, Retries: retries, Backoff: 5 * time.Millisecond, UserAgent: "inventory-agent/test"})
}

func TestSendSuccessHeadersAndBody(t *testing.T) {
	var got capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.headers = r.Header.Clone()
		got.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	res := newSender(srv.URL, 2).Send(context.Background(), gz(t, `{"hello":"world"}`))
	if !res.OK() || res.HTTPStatus != 202 || res.Attempts != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got.headers.Get("Content-Type") != "application/json" || got.headers.Get("Content-Encoding") != "gzip" {
		t.Fatalf("unexpected headers: %v", got.headers)
	}
	if got.headers.Get("User-Agent") != "inventory-agent/test" {
		t.Fatalf("unexpected user agent: %q", got.headers.Get("User-Agent"))
	}
	// v1: no authentication header of any kind.
	if _, present := got.headers["Authorization"]; present {
		t.Fatal("Authorization header must not be sent in v1")
	}
	zr, err := gzip.NewReader(bytes.NewReader(got.body))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	plain, _ := io.ReadAll(zr)
	if string(plain) != `{"hello":"world"}` {
		t.Fatalf("unexpected body: %s", plain)
	}
}

func TestSendRetriesOn500ThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	res := newSender(srv.URL, 2).Send(context.Background(), gz(t, "{}"))
	if !res.OK() || res.HTTPStatus != 200 || res.Attempts != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestSendRetriesOn429AndGivesUp(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	res := newSender(srv.URL, 2).Send(context.Background(), gz(t, "{}"))
	if res.OK() || res.HTTPStatus != 429 || res.Attempts != 3 || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("unexpected result: %+v calls=%d", res, calls)
	}
}

func TestSendNeverFollowsRedirects(t *testing.T) {
	var reachedTarget int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reachedTarget, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		var calls int32
		src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			http.Redirect(w, r, target.URL+"/elsewhere", code)
		}))
		res := newSender(src.URL, 2).Send(context.Background(), gz(t, "{}"))
		src.Close()
		if res.OK() || res.Err == nil || res.HTTPStatus != code {
			t.Fatalf("%d: expected a failed result with the 3xx status, got %+v", code, res)
		}
		if res.Attempts != 1 || atomic.LoadInt32(&calls) != 1 {
			t.Fatalf("%d: a redirect must not be retried, got attempts=%d calls=%d", code, res.Attempts, calls)
		}
		var se *StatusError
		if !errors.As(res.Err, &se) || se.Code != code {
			t.Fatalf("%d: expected StatusError, got %T %v", code, res.Err, res.Err)
		}
	}
	if atomic.LoadInt32(&reachedTarget) != 0 {
		t.Fatalf("redirect target received %d requests; the POST must never be replayed or downgraded", reachedTarget)
	}
}

func TestSendRedirectNeverDowngradesHTTPS(t *testing.T) {
	// An https endpoint redirecting to plain http must not be followed: the
	// only request on the wire is the original TLS one.
	var plainHits int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&plainHits, 1)
	}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusMovedPermanently)
	}))
	defer tls.Close()

	s := New(Options{URL: tls.URL, Retries: 1, Backoff: time.Millisecond, Transport: tls.Client().Transport})
	res := s.Send(context.Background(), gz(t, "{}"))
	if res.OK() || res.HTTPStatus != http.StatusMovedPermanently {
		t.Fatalf("unexpected result: %+v", res)
	}
	if atomic.LoadInt32(&plainHits) != 0 {
		t.Fatal("https request was downgraded to http via redirect")
	}
}

func TestSendDoesNotRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	res := newSender(srv.URL, 3).Send(context.Background(), gz(t, "{}"))
	if res.OK() || res.HTTPStatus != 400 || res.Attempts != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestSendTimeoutIsRetriedThenFails(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	s := New(Options{URL: srv.URL, Timeout: 50 * time.Millisecond, Retries: 1, Backoff: time.Millisecond})
	start := time.Now()
	res := s.Send(context.Background(), gz(t, "{}"))
	if res.OK() || res.HTTPStatus != 0 || res.Attempts != 2 || res.Err == nil {
		t.Fatalf("unexpected result: %+v", res)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not enforced")
	}
}

func TestSendConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	res := newSender(url, 1).Send(context.Background(), gz(t, "{}"))
	if res.OK() || res.Attempts != 2 || res.HTTPStatus != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestSendErrorsNeverContainURLSecrets: connection refused, timeout and
// non-2xx errors must not echo the path, query string or userinfo of the
// ingest URL (they end up in the send_failed log line).
func TestSendErrorsNeverContainURLSecrets(t *testing.T) {
	// Canary values stand in for anything sensitive a URL could carry. The
	// userinfo is attached at runtime so the source never contains a
	// credential-bearing URL literal.
	const canaryPath = "/ingest/CANARY_PATH_SEGMENT"
	const canaryQuery = "token=CANARY_QUERY_VALUE"
	canaryUser := url.UserPassword("canary-user", "CANARY_USERINFO")
	forbidden := []string{"CANARY_PATH_SEGMENT", "CANARY_QUERY_VALUE", "CANARY_USERINFO", "canary-user", canaryPath}

	hang := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hang }))
	defer slow.Close() // runs last: Close waits for handlers, so hang must be closed first
	defer close(hang)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
	defer failing.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	withCanaries := func(base string) string {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		u.User = canaryUser
		u.Path = canaryPath
		u.RawQuery = canaryQuery
		return u.String()
	}
	unparseable := (&url.URL{Scheme: "http", User: canaryUser, Host: "bad host", Path: canaryPath, RawQuery: canaryQuery}).String()
	cases := map[string]string{
		"connection refused": withCanaries(closedURL),
		"timeout":            withCanaries(slow.URL),
		"non-2xx":            withCanaries(failing.URL),
		"unparseable":        unparseable,
	}
	for name, u := range cases {
		res := New(Options{URL: u, Timeout: 50 * time.Millisecond, Retries: 0}).Send(context.Background(), gz(t, "{}"))
		if res.OK() || res.Err == nil {
			t.Fatalf("%s: expected failure, got %+v", name, res)
		}
		text := res.Err.Error()
		for _, f := range forbidden {
			if strings.Contains(text, f) {
				t.Errorf("%s: error text leaks %q: %s", name, f, text)
			}
		}
	}
}

func TestSendRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := New(Options{URL: srv.URL, Retries: 5, Backoff: time.Hour}).Send(ctx, gz(t, "{}"))
	if res.OK() || res.Attempts > 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestBearerTokenSlotIsWiredButOffByDefault(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
	}))
	defer srv.Close()
	New(Options{URL: srv.URL}).Send(context.Background(), gz(t, "{}"))
	if auth != "" {
		t.Fatalf("expected no Authorization by default, got %q", auth)
	}
	New(Options{URL: srv.URL, BearerToken: "t0k3n"}).Send(context.Background(), gz(t, "{}"))
	if auth != "Bearer t0k3n" {
		t.Fatalf("expected bearer header when configured, got %q", auth)
	}
}

func sampleEnvelope(rows int) *schema.Envelope {
	env := &schema.Envelope{SchemaVersion: 1, CaptainDomain: "x.onglueops.com", RunID: "20261001050000_deadbeef", CollectedAt: "2026-10-01T05:00:00.000Z"}
	env.Datasets.Cluster = schema.ClusterSection{SchemaVersion: 1, Status: "ok", Data: &schema.ClusterData{Nodes: []schema.Node{}}}
	env.Datasets.HelmReleases = schema.HelmReleasesSection{SchemaVersion: 1, Status: "ok", Data: []schema.HelmRelease{}}
	env.Datasets.PodImages = schema.PodImagesSection{SchemaVersion: 1, Status: "ok", NamespacesRequested: []string{"a"}, NamespacesDenied: []string{}}
	for i := 0; i < rows; i++ {
		// Random-looking content so gzip cannot collapse the rows.
		img := "registry.example.com/team/service-" + string(rune('a'+i%26)) + "/image:" + time.Unix(int64(i)*7919, 0).UTC().Format(time.RFC3339Nano)
		env.Datasets.PodImages.Data = append(env.Datasets.PodImages.Data, schema.PodImage{
			Namespace: "ns", PodName: "pod-" + img, WorkloadKind: "Deployment", WorkloadName: img, ContainerName: "c", ContainerType: "app", Image: img,
		})
	}
	return env
}

func TestPackWithinCap(t *testing.T) {
	env := sampleEnvelope(10)
	body, dropped, err := Pack(env, PackOptions{MaxBytes: 1 << 20, Gzip: true})
	if err != nil || dropped != 0 || env.Datasets.PodImages.Truncated || len(env.Datasets.PodImages.Data) != 10 {
		t.Fatalf("unexpected: dropped=%d err=%v env=%+v", dropped, err, env.Datasets.PodImages)
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var back schema.Envelope
	if err := json.NewDecoder(zr).Decode(&back); err != nil {
		t.Fatal(err)
	}
	if len(back.Datasets.PodImages.Data) != 10 {
		t.Fatalf("round trip lost rows: %d", len(back.Datasets.PodImages.Data))
	}
}

func TestPackTruncatesToSizeCap(t *testing.T) {
	env := sampleEnvelope(2000)
	full, _, err := Pack(sampleEnvelope(2000), PackOptions{Gzip: true})
	if err != nil {
		t.Fatal(err)
	}
	cap := len(full) / 3
	body, dropped, err := Pack(env, PackOptions{MaxBytes: cap, Gzip: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > cap {
		t.Fatalf("body %d exceeds cap %d", len(body), cap)
	}
	if dropped == 0 || !env.Datasets.PodImages.Truncated || len(env.Datasets.PodImages.Data)+dropped != 2000 {
		t.Fatalf("unexpected truncation: dropped=%d kept=%d truncated=%v", dropped, len(env.Datasets.PodImages.Data), env.Datasets.PodImages.Truncated)
	}
	if len(env.Datasets.PodImages.Data) == 0 {
		t.Fatal("expected some rows to survive a cap of one third")
	}
}

func TestPackWithZeroRowsStillTooLargeReturnsBody(t *testing.T) {
	env := sampleEnvelope(0)
	body, dropped, err := Pack(env, PackOptions{MaxBytes: 10, Gzip: true})
	if err != nil || dropped != 0 || len(body) == 0 || env.Datasets.PodImages.Truncated {
		t.Fatalf("unexpected: dropped=%d err=%v len=%d", dropped, err, len(body))
	}
}

func TestPackWithoutGzipReturnsPlainJSONAndRespectsCap(t *testing.T) {
	env := sampleEnvelope(50)
	body, dropped, err := Pack(env, PackOptions{MaxBytes: 1 << 20, Gzip: false})
	if err != nil || dropped != 0 {
		t.Fatalf("unexpected: dropped=%d err=%v", dropped, err)
	}
	if _, gzErr := gzip.NewReader(bytes.NewReader(body)); gzErr == nil {
		t.Fatal("body must not be gzipped")
	}
	var back schema.Envelope
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("body is not plain JSON: %v", err)
	}
	if back.RunID != env.RunID || len(back.Datasets.PodImages.Data) != 50 {
		t.Fatalf("round trip mismatch: %+v", back)
	}

	// The cap applies to the uncompressed bytes.
	env = sampleEnvelope(500)
	full, _, _ := Pack(sampleEnvelope(500), PackOptions{Gzip: false})
	cap := len(full) / 2
	body, dropped, err = Pack(env, PackOptions{MaxBytes: cap, Gzip: false})
	if err != nil || len(body) > cap || dropped == 0 || !env.Datasets.PodImages.Truncated {
		t.Fatalf("cap not applied to plain body: len=%d cap=%d dropped=%d truncated=%v err=%v",
			len(body), cap, dropped, env.Datasets.PodImages.Truncated, err)
	}
	if err := json.Unmarshal(body, &back); err != nil || !back.Datasets.PodImages.Truncated {
		t.Fatalf("truncated plain body must still be valid JSON: %v", err)
	}
}

func TestSendWithoutGzipSendsPlainJSONAndNoContentEncoding(t *testing.T) {
	var got capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.headers = r.Header.Clone()
		got.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	body, _, err := Pack(sampleEnvelope(3), PackOptions{Gzip: false})
	if err != nil {
		t.Fatal(err)
	}
	res := New(Options{URL: srv.URL, Encoding: EncodingIdentity}).Send(context.Background(), body)
	if !res.OK() {
		t.Fatalf("unexpected result: %+v", res)
	}
	if _, present := got.headers["Content-Encoding"]; present {
		t.Fatalf("Content-Encoding must be absent, got %q", got.headers.Get("Content-Encoding"))
	}
	if got.headers.Get("Content-Type") != "application/json" {
		t.Fatalf("unexpected content type %q", got.headers.Get("Content-Type"))
	}
	var back schema.Envelope
	if err := json.Unmarshal(got.body, &back); err != nil || len(back.Datasets.PodImages.Data) != 3 {
		t.Fatalf("server did not receive valid envelope JSON: %v", err)
	}
}
