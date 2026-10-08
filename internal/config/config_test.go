package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HelmNamespace != "glueops-core" || cfg.MaxPodRows != 5000 || cfg.MaxGzipBytes != 2097152 ||
		cfg.HTTPTimeout != 10*time.Second || cfg.Retries != 2 || cfg.DevMode || cfg.LogLevel != "info" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if len(cfg.PodNamespaces) != 2 || cfg.PodNamespaces[0] != "kube-system" {
		t.Fatalf("unexpected namespaces: %v", cfg.PodNamespaces)
	}
}

func TestLoadParsesEverything(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"CAPTAIN_DOMAIN":                 "nonprod.foo.onglueops.com",
		"INGEST_URL":                     "https://ingest.example.com/v1",
		"POD_NAMESPACES":                 " kube-system, glueops-core ,glueops-core, ,glueops-core-argocd-extension-backend",
		"HELM_NAMESPACE":                 "other",
		"GLUEOPS_PLATFORM_CHART_VERSION": "0.80.2",
		"DEV_MODE":                       "true",
		"LOG_LEVEL":                      "debug",
		"MAX_POD_ROWS":                   "10",
		"MAX_GZIP_BYTES":                 "1024",
		"HTTP_TIMEOUT":                   "3s",
		"RETRIES":                        "1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.PodNamespaces, ","); got != "kube-system,glueops-core,glueops-core-argocd-extension-backend" {
		t.Fatalf("namespaces not trimmed/deduped: %q", got)
	}
	if cfg.HelmNamespace != "other" || cfg.PlatformChartVersion != "0.80.2" || !cfg.DevMode || cfg.LogLevel != "debug" ||
		cfg.MaxPodRows != 10 || cfg.MaxGzipBytes != 1024 || cfg.HTTPTimeout != 3*time.Second || cfg.Retries != 1 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	cases := map[string]map[string]string{
		"missing captain domain": {},
		"placeholder domain":     {"CAPTAIN_DOMAIN": "PLACEHOLDER.onglueops.com"},
		"bad bool":               {"CAPTAIN_DOMAIN": "x.com", "DEV_MODE": "maybe"},
		"bad rows":               {"CAPTAIN_DOMAIN": "x.com", "MAX_POD_ROWS": "0"},
		"bad bytes":              {"CAPTAIN_DOMAIN": "x.com", "MAX_GZIP_BYTES": "lots"},
		"bad timeout":            {"CAPTAIN_DOMAIN": "x.com", "HTTP_TIMEOUT": "-1s"},
		"bad retries":            {"CAPTAIN_DOMAIN": "x.com", "RETRIES": "99"},
	}
	for name, m := range cases {
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestValidateIngestURL(t *testing.T) {
	cases := []struct {
		url  string
		dev  bool
		want bool
	}{
		{"", false, true},
		{"https://ingest.example.com/v1", false, true},
		{"http://localhost:8080/", false, false},
		{"http://localhost:8080/", true, true},
		{"ftp://x", true, false},
		{"not a url", false, false},
		{"https://", false, false},
		{"https://canary-user:" + "CANARY_USERINFO" + "@ingest.example.com/v1", false, false},
		{"https://user@ingest.example.com/v1", false, false},
		{"http://canary-user:" + "CANARY_USERINFO" + "@localhost:8080/", true, false},
	}
	for _, c := range cases {
		err := ValidateIngestURL(c.url, c.dev)
		if (err == nil) != c.want {
			t.Errorf("ValidateIngestURL(%q, dev=%v) = %v, want ok=%v", c.url, c.dev, err, c.want)
		}
	}
}
