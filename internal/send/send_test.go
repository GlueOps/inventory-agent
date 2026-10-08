package send

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	body, dropped, err := Pack(env, 1<<20)
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
	full, _, err := Pack(sampleEnvelope(2000), 0)
	if err != nil {
		t.Fatal(err)
	}
	cap := len(full) / 3
	body, dropped, err := Pack(env, cap)
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
	body, dropped, err := Pack(env, 10)
	if err != nil || dropped != 0 || len(body) == 0 || env.Datasets.PodImages.Truncated {
		t.Fatalf("unexpected: dropped=%d err=%v len=%d", dropped, err, len(body))
	}
}
