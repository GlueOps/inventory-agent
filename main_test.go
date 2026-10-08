package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
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
		"no endpoint":     {map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com"}, "no_endpoint_configured"},
		"invalid config":  {map[string]string{}, "invalid_config"},
		"placeholder":     {map[string]string{"CAPTAIN_DOMAIN": "placeholder.onglueops.com", "INGEST_URL": "https://x"}, "invalid_config"},
		"non-https url":   {map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "INGEST_URL": "http://x"}, "invalid_ingest_url"},
		"bad env numbers": {map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "MAX_POD_ROWS": "-1"}, "invalid_config"},
		"empty ns list":   {map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "POD_NAMESPACES": ""}, "invalid_config"},
	}
	for name, c := range cases {
		var out bytes.Buffer
		if code := run(envFrom(c.env), &out); code != 0 {
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
