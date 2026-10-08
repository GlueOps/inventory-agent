package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HelmNamespace != "glueops-core" || cfg.MaxPodRows != 5000 || cfg.MaxGzipBytes != 2097152 ||
		cfg.HTTPTimeout != 10*time.Second || cfg.Retries != 2 || cfg.DevMode || !cfg.SendGzip || cfg.LogLevel != "info" {
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
		"bad send gzip":          {"CAPTAIN_DOMAIN": "x.com", "SEND_GZIP": "garbage"},
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

func TestLoadSendGzip(t *testing.T) {
	for value, want := range map[string]bool{"": true, "true": true, "TRUE": true, "1": true, "false": false, "no": false, "0": false} {
		cfg, err := Load(env(map[string]string{"CAPTAIN_DOMAIN": "x.com", "SEND_GZIP": value}))
		if err != nil || cfg.SendGzip != want {
			t.Errorf("SEND_GZIP=%q: got %v, %v; want %v", value, cfg.SendGzip, err, want)
		}
	}
	if _, err := Load(env(map[string]string{"CAPTAIN_DOMAIN": "x.com", "SEND_GZIP": "garbage"})); err == nil || !strings.Contains(err.Error(), "SEND_GZIP") {
		t.Fatalf("expected SEND_GZIP error, got %v", err)
	}
}

func TestLoadUnsetVersusEmptyNamespaces(t *testing.T) {
	base := map[string]string{"CAPTAIN_DOMAIN": "x.com"}

	// Unset: defaults apply.
	cfg, err := Load(env(base))
	if err != nil || cfg.HelmNamespace != DefaultHelmNamespace || len(cfg.PodNamespaces) != 2 {
		t.Fatalf("unset should default: %+v %v", cfg, err)
	}

	// Set but empty (or effectively empty): invalid_config.
	for name, m := range map[string]map[string]string{
		"POD_NAMESPACES empty":       {"CAPTAIN_DOMAIN": "x.com", "POD_NAMESPACES": ""},
		"POD_NAMESPACES whitespace":  {"CAPTAIN_DOMAIN": "x.com", "POD_NAMESPACES": "   "},
		"POD_NAMESPACES only commas": {"CAPTAIN_DOMAIN": "x.com", "POD_NAMESPACES": ", ,,"},
		"HELM_NAMESPACE empty":       {"CAPTAIN_DOMAIN": "x.com", "HELM_NAMESPACE": ""},
		"HELM_NAMESPACE whitespace":  {"CAPTAIN_DOMAIN": "x.com", "HELM_NAMESPACE": " \t"},
	} {
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: expected invalid_config error", name)
		} else if !strings.Contains(err.Error(), "is set but") {
			t.Errorf("%s: unexpected error text %q", name, err)
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
