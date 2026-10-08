package main

import (
	"path/filepath"
	"testing"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestRunAlwaysExitsZero drives the real entrypoint with no cluster and no
// endpoint, with invalid config, and with an unusable kubeconfig but a bad
// URL; every path must return 0.
func TestRunAlwaysExitsZero(t *testing.T) {
	missingKubeconfig := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("KUBECONFIG", missingKubeconfig)
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	cases := map[string]map[string]string{
		"no endpoint":     {"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com"},
		"invalid config":  {},
		"placeholder":     {"CAPTAIN_DOMAIN": "placeholder.onglueops.com", "INGEST_URL": "https://x"},
		"non-https url":   {"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "INGEST_URL": "http://x"},
		"bad env numbers": {"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "MAX_POD_ROWS": "-1"},
	}
	for name, env := range cases {
		if code := run(envFrom(env)); code != 0 {
			t.Errorf("%s: exit code %d, want 0", name, code)
		}
	}
}
