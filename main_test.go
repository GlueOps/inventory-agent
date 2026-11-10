package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// TestRunAlwaysExitsZeroWithOneSummary drives the real entrypoint with no
// cluster and no endpoint, with invalid config, and with an unusable
// kubeconfig but a bad URL; every path must return 0 and log exactly one
// "run summary" line with the expected status.
func TestRunAlwaysExitsZeroWithOneSummary(t *testing.T) {
	missingKubeconfig := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("KUBECONFIG", missingKubeconfig)
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	cases := map[string]struct {
		env    map[string]string
		status string
	}{
		"no endpoint":      {map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "INGEST_URL": ""}, "no_endpoint_configured"},
		"invalid config":   {map[string]string{}, "invalid_config"},
		"placeholder":      {map[string]string{"CAPTAIN_DOMAIN": "placeholder.onglueops.com", "INGEST_URL": "https://x"}, "invalid_config"},
		"non-https url":    {map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "INGEST_URL": "http://x"}, "invalid_ingest_url"},
		"bad env numbers":  {map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "MAX_POD_ROWS": "-1"}, "invalid_config"},
		"empty ns pattern": {map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "NAMESPACE_PATTERN": ""}, "invalid_config"},
		"bad ns pattern":   {map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "NAMESPACE_PATTERN": "glueops-core("}, "invalid_config"},
	}
	for name, c := range cases {
		var out bytes.Buffer
		if code := run(envFrom(c.env), &out, noSleep(t)); code != 0 {
			t.Errorf("%s: exit code %d, want 0", name, code)
		}
		logs := out.String()
		if n := strings.Count(logs, `"msg":"run summary"`); n != 1 {
			t.Errorf("%s: expected exactly one summary line, got %d:\n%s", name, n, logs)
		}
		if !strings.Contains(logs, `"status":"`+c.status+`"`) {
			t.Errorf("%s: expected status %q in summary:\n%s", name, c.status, logs)
		}
		if c.status == "invalid_config" && !strings.Contains(logs, `"reason":"invalid_config"`) {
			t.Errorf("%s: expected reason invalid_config in summary:\n%s", name, logs)
		}
		if !strings.Contains(logs, `"run_id":"`) {
			t.Errorf("%s: summary must carry a run_id:\n%s", name, logs)
		}
	}
}

// noSleep fails the test if the run tries to delay its start.
func noSleep(t *testing.T) func(time.Duration) {
	return func(d time.Duration) { t.Helper(); t.Errorf("unexpected start delay %s", d) }
}

// TestRunStartJitter: with START_JITTER set, the run sleeps once for less
// than the bound and says so; unset, or with nothing to send, it never sleeps.
func TestRunStartJitter(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	base := map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "INGEST_URL": srv.URL, "DEV_MODE": "true"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	t.Run("delays below the bound", func(t *testing.T) {
		var slept []time.Duration
		var out bytes.Buffer
		run(envFrom(with(map[string]string{"START_JITTER": "5m"})), &out, func(d time.Duration) { slept = append(slept, d) })
		if len(slept) != 1 || slept[0] < 0 || slept[0] >= 5*time.Minute {
			t.Fatalf("slept %v, want one delay in [0, 5m)", slept)
		}
		logs := out.String()
		if !strings.Contains(logs, `"msg":"start delayed"`) || !strings.Contains(logs, `"max_ms":300000`) {
			t.Fatalf("missing start delayed line:\n%s", logs)
		}
		if n := strings.Count(logs, `"msg":"run summary"`); n != 1 {
			t.Fatalf("expected one summary line, got %d:\n%s", n, logs)
		}
	})
	t.Run("unset never sleeps", func(t *testing.T) {
		var out bytes.Buffer
		run(envFrom(base), &out, noSleep(t))
		if strings.Contains(out.String(), "start delayed") {
			t.Fatalf("unexpected start delayed line:\n%s", out.String())
		}
	})
	t.Run("no endpoint never sleeps", func(t *testing.T) {
		var out bytes.Buffer
		run(envFrom(with(map[string]string{"START_JITTER": "5m", "INGEST_URL": ""})), &out, noSleep(t))
	})
	t.Run("invalid jitter is invalid_config", func(t *testing.T) {
		var out bytes.Buffer
		run(envFrom(with(map[string]string{"START_JITTER": "15m"})), &out, noSleep(t))
		if !strings.Contains(out.String(), `"status":"invalid_config"`) {
			t.Fatalf("want invalid_config:\n%s", out.String())
		}
	})
}
