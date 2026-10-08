package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/glueops/inventory-agent/internal/config"
	"github.com/glueops/inventory-agent/internal/logging"
	"github.com/glueops/inventory-agent/internal/schema"
	"github.com/glueops/inventory-agent/internal/send"
)

const (
	secretMarker = "SECRET_MARKER_argocd_admin_password"
	podEnvMarker = "POD_ENV_MARKER_aws_secret"
)

type fakeSender struct {
	result send.Result
	body   []byte
	calls  int
	panics bool
}

func (f *fakeSender) Send(_ context.Context, body []byte) send.Result {
	f.calls++
	if f.panics {
		panic("sender exploded")
	}
	f.body = body
	return f.result
}

func fixtureClient() *fake.Clientset {
	ctrl := true
	client := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-abc")}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.35.8"}}},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "argocd-secret", Namespace: "glueops-core"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"admin.password": []byte(secretMarker)},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-abc12-xyz", Namespace: "kube-system",
				Labels:          map[string]string{"pod-template-hash": "abc12"},
				OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc12", Controller: &ctrl}}},
			Spec: corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{
				Name: "web", Image: "ghcr.io/glueops/web:1.0.0",
				Env: []corev1.EnvVar{{Name: "AWS_SECRET_ACCESS_KEY", Value: podEnvMarker}},
			}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
				Name: "web", ImageID: "ghcr.io/glueops/web@sha256:900f9c109f7a33545d3c811516e8376df9019147b750f5ce3e254468769176ea",
			}}},
		},
	)
	client.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: "35", GitVersion: "v1.35.8", Platform: "linux/amd64"}
	return client
}

func baseConfig() config.Config {
	return config.Config{
		CaptainDomain: "nonprod.foobar.onglueops.com",
		IngestURL:     "https://ingest.example.com/v1/inventory",
		PodNamespaces: []string{"kube-system", "glueops-core"},
		HelmNamespace: "glueops-core",
		MaxPodRows:    5000,
		MaxGzipBytes:  2 << 20,
		HTTPTimeout:   time.Second,
		Retries:       1,
	}
}

func decodeBody(t *testing.T, body []byte) schema.Envelope {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("body not gzip: %v", err)
	}
	var env schema.Envelope
	if err := json.NewDecoder(zr).Decode(&env); err != nil {
		t.Fatal(err)
	}
	return env
}

func runWith(t *testing.T, cfg config.Config, client *fake.Clientset, sender *fakeSender) (int, Summary, string) {
	t.Helper()
	var logs bytes.Buffer
	log := logging.New("debug", &logs)
	deps := Deps{Sender: sender, Version: "v0.1.0-test", Now: func() time.Time { return time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC) }}
	if client != nil {
		deps.Client = client
	}
	code, summary := Run(context.Background(), cfg, deps, log)
	return code, summary, logs.String()
}

func TestRunHappyPath(t *testing.T) {
	sender := &fakeSender{result: send.Result{HTTPStatus: 200, Attempts: 1}}
	code, summary, logs := runWith(t, baseConfig(), fixtureClient(), sender)
	if code != 0 || summary.Status != StatusOK || summary.HTTPStatus != 200 || summary.BytesSent != len(sender.body) || summary.BytesSent == 0 {
		t.Fatalf("unexpected: code=%d summary=%+v", code, summary)
	}
	if !regexp.MustCompile(`^20261001050000_[0-9a-f]{8}$`).MatchString(summary.RunID) {
		t.Fatalf("bad run_id %q", summary.RunID)
	}
	env := decodeBody(t, sender.body)
	if env.RunID != summary.RunID || *env.ClusterUID != "uid-abc" || env.CollectorVersion != "v0.1.0-test" || env.CollectedAt != "2026-10-01T05:00:00.000Z" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	if env.Datasets.Cluster.Status != "ok" || env.Datasets.HelmReleases.Status != "ok" || env.Datasets.PodImages.Status != "ok" || len(env.Datasets.PodImages.Data) != 1 {
		t.Fatalf("unexpected sections: %+v", env.Datasets)
	}
	if !strings.Contains(logs, `"msg":"run summary"`) || !strings.Contains(logs, `"status":"ok"`) || !strings.Contains(logs, `"section_helm_releases":"ok"`) {
		t.Fatalf("summary line missing or wrong:\n%s", logs)
	}
	if strings.Count(logs, `"msg":"run summary"`) != 1 {
		t.Fatal("exactly one summary line expected")
	}
}

func TestRunRBACDeniedOnSecretsIsIsolated(t *testing.T) {
	client := fixtureClient()
	client.PrependReactor("list", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(k8sschema.GroupResource{Resource: "secrets"}, "", errors.New("denied"))
	})
	sender := &fakeSender{result: send.Result{HTTPStatus: 200, Attempts: 1}}
	code, summary, logs := runWith(t, baseConfig(), client, sender)
	if code != 0 || summary.Status != StatusPartial {
		t.Fatalf("unexpected: code=%d summary=%+v", code, summary)
	}
	env := decodeBody(t, sender.body)
	h := env.Datasets.HelmReleases
	if h.Status != "error" || h.Error != "rbac_denied" || h.Data != nil {
		t.Fatalf("expected helm rbac_denied, got %+v", h)
	}
	if env.Datasets.Cluster.Status != "ok" || env.Datasets.PodImages.Status != "ok" {
		t.Fatalf("other sections must be ok: %+v", env.Datasets)
	}
	if !strings.Contains(logs, `"section_helm_releases":"error:rbac_denied"`) {
		t.Fatalf("summary should name the failed section:\n%s", logs)
	}
}

func TestRunExitsZeroOnEveryFailureMode(t *testing.T) {
	cases := map[string]struct {
		cfg    func() config.Config
		client *fake.Clientset
		sender *fakeSender
		status string
	}{
		"no endpoint": {
			cfg:    func() config.Config { c := baseConfig(); c.IngestURL = ""; return c },
			client: fixtureClient(), sender: &fakeSender{}, status: StatusNoEndpointConfigured,
		},
		"userinfo in url": {
			cfg: func() config.Config {
				c := baseConfig()
				c.IngestURL = "https://canary-user:" + "CANARY_USERINFO" + "@ingest.example.com/v1"
				return c
			},
			client: fixtureClient(), sender: &fakeSender{}, status: StatusInvalidIngestURL,
		},
		"http without dev mode": {
			cfg:    func() config.Config { c := baseConfig(); c.IngestURL = "http://ingest.example.com"; return c },
			client: fixtureClient(), sender: &fakeSender{}, status: StatusInvalidIngestURL,
		},
		"receiver down": {
			cfg: baseConfig, client: fixtureClient(),
			sender: &fakeSender{result: send.Result{HTTPStatus: 503, Attempts: 2, Err: &send.StatusError{Code: 503}}},
			status: StatusSendFailed,
		},
		"receiver unreachable": {
			cfg: baseConfig, client: fixtureClient(),
			sender: &fakeSender{result: send.Result{HTTPStatus: 0, Attempts: 3, Err: errors.New("connection refused")}},
			status: StatusSendFailed,
		},
		"no kube client": {
			cfg: baseConfig, client: nil,
			sender: &fakeSender{result: send.Result{HTTPStatus: 200, Attempts: 1}},
			status: StatusPartial,
		},
		"sender panics": {
			cfg: baseConfig, client: fixtureClient(),
			sender: &fakeSender{panics: true}, status: StatusInternalError,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			code, summary, logs := runWith(t, c.cfg(), c.client, c.sender)
			if code != 0 {
				t.Fatalf("exit code %d, want 0", code)
			}
			if summary.Status != c.status {
				t.Fatalf("status %q, want %q", summary.Status, c.status)
			}
			if strings.Count(logs, `"msg":"run summary"`) != 1 {
				t.Fatalf("expected exactly one summary line:\n%s", logs)
			}
			if !strings.Contains(logs, `"reason":"`) {
				t.Fatalf("expected a reason code in logs:\n%s", logs)
			}
		})
	}
}

func TestRunSummaryCountsSkippedHelmSecrets(t *testing.T) {
	client := fixtureClient()
	_ = client.Tracker().Add(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.bad.v1", Namespace: "glueops-core", Labels: map[string]string{"owner": "helm"}},
		Type:       "helm.sh/release.v1",
		Data:       map[string][]byte{"release": []byte("garbage")},
	})
	sender := &fakeSender{result: send.Result{HTTPStatus: 200, Attempts: 1}}
	_, summary, logs := runWith(t, baseConfig(), client, sender)
	if summary.HelmDecodeFailed != 1 || !strings.Contains(logs, `"helm_decode_failed":1`) {
		t.Fatalf("expected helm_decode_failed=1 in summary, got %+v\n%s", summary, logs)
	}
}

func TestRunNoEndpointDoesNotCollectOrSend(t *testing.T) {
	client := fixtureClient()
	var listed bool
	client.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		listed = true
		return false, nil, nil
	})
	cfg := baseConfig()
	cfg.IngestURL = ""
	sender := &fakeSender{}
	_, _, logs := runWith(t, cfg, client, sender)
	if listed || sender.calls != 0 {
		t.Fatal("nothing should be collected or sent without an endpoint")
	}
	if !strings.Contains(logs, `"reason":"no_endpoint_configured"`) {
		t.Fatalf("expected no_endpoint_configured:\n%s", logs)
	}
}

func TestRunDevModeAllowsHTTP(t *testing.T) {
	cfg := baseConfig()
	cfg.IngestURL = "http://localhost:9999/ingest"
	cfg.DevMode = true
	sender := &fakeSender{result: send.Result{HTTPStatus: 200, Attempts: 1}}
	code, summary, _ := runWith(t, cfg, fixtureClient(), sender)
	if code != 0 || summary.Status != StatusOK || sender.calls != 1 {
		t.Fatalf("unexpected: %+v", summary)
	}
}

func TestRunSizeCapTruncatesPodImages(t *testing.T) {
	client := fixtureClient()
	ctrl := true
	for i := 0; i < 300; i++ {
		sum := sha256.Sum256([]byte{byte(i), byte(i >> 8)})
		name := "filler-" + hex.EncodeToString(sum[:12])
		_ = client.Tracker().Add(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "glueops-core",
				OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: "filler", Controller: &ctrl}}},
			Spec:   corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "c", Image: "img/" + name}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		})
	}
	cfg := baseConfig()
	cfg.MaxGzipBytes = 3072
	sender := &fakeSender{result: send.Result{HTTPStatus: 200, Attempts: 1}}
	code, summary, logs := runWith(t, cfg, client, sender)
	if code != 0 || summary.Status != StatusOK {
		t.Fatalf("unexpected: %+v", summary)
	}
	if len(sender.body) > cfg.MaxGzipBytes {
		t.Fatalf("body %d exceeds cap", len(sender.body))
	}
	env := decodeBody(t, sender.body)
	if !env.Datasets.PodImages.Truncated || env.Datasets.PodImages.Status != "ok" || len(env.Datasets.PodImages.Data) >= 301 {
		t.Fatalf("expected truncated ok section, got truncated=%v rows=%d", env.Datasets.PodImages.Truncated, len(env.Datasets.PodImages.Data))
	}
	if !summary.Truncated || !strings.Contains(logs, `"reason":"payload_truncated"`) {
		t.Fatalf("expected truncation to be reported:\n%s", logs)
	}
}

func TestRunRedaction(t *testing.T) {
	// Secrets readable, pods with env values present: nothing of either may
	// reach the payload or the logs, at debug level.
	sender := &fakeSender{result: send.Result{HTTPStatus: 200, Attempts: 1}}
	_, _, logs := runWith(t, baseConfig(), fixtureClient(), sender)
	for _, marker := range []string{secretMarker, podEnvMarker, "AWS_SECRET_ACCESS_KEY", "admin.password"} {
		if bytes.Contains(sender.body, []byte(marker)) {
			t.Errorf("payload contains %q", marker)
		}
		if strings.Contains(logs, marker) {
			t.Errorf("logs contain %q", marker)
		}
	}
	// Decode and check the plaintext too (the gzip check above is only a sanity check).
	raw, _ := json.Marshal(decodeBody(t, sender.body))
	for _, marker := range []string{secretMarker, podEnvMarker, "AWS_SECRET_ACCESS_KEY"} {
		if bytes.Contains(raw, []byte(marker)) {
			t.Errorf("decoded payload contains %q", marker)
		}
	}
}

// TestRunSendFailedLogNeverContainsURLSecrets uses the real sender against
// a closed port and a hanging server; the send_failed line must not echo
// the ingest URL's path or query string.
func TestRunSendFailedLogNeverContainsURLSecrets(t *testing.T) {
	hang := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hang }))
	defer slow.Close() // runs last: Close waits for handlers, so hang must be closed first
	defer close(hang)
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	for name, base := range map[string]string{"connection refused": closedURL, "timeout": slow.URL} {
		cfg := baseConfig()
		cfg.DevMode = true
		cfg.Retries = 0
		cfg.HTTPTimeout = 50 * time.Millisecond
		cfg.IngestURL = base + "/ingest/SECRET_PATH_SEGMENT?token=SECRET_QUERY_TOKEN"
		var logs bytes.Buffer
		code, summary := Run(context.Background(), cfg, Deps{Client: fixtureClient(), Version: "t"}, logging.New("debug", &logs))
		if code != 0 || summary.Status != StatusSendFailed {
			t.Fatalf("%s: unexpected: code=%d summary=%+v", name, code, summary)
		}
		for _, f := range []string{"SECRET_PATH_SEGMENT", "SECRET_QUERY_TOKEN", "/ingest"} {
			if strings.Contains(logs.String(), f) {
				t.Errorf("%s: logs leak %q:\n%s", name, f, logs.String())
			}
		}
	}
}

func TestNewRunIDFormat(t *testing.T) {
	ts := time.Date(2026, 10, 1, 5, 0, 0, 0, time.FixedZone("plus2", 2*3600))
	id := NewRunID(ts)
	if !regexp.MustCompile(`^20261001030000_[0-9a-f]{8}$`).MatchString(id) {
		t.Fatalf("bad run id %q (must be UTC)", id)
	}
	if NewRunID(ts) == id {
		t.Fatal("run ids must differ between runs")
	}
}
