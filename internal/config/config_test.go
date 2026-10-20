package config

import (
	"net/url"
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
	if got := cfg.NamespacePattern.String(); got != "^(?:kube-system|glueops-core(-.*)?)$" {
		t.Fatalf("unexpected default namespace pattern: %q", got)
	}
	if cfg.IngestURL != "https://inventory-agent.glueopshosted.com/" {
		t.Fatalf("unexpected default ingest URL: %q", cfg.IngestURL)
	}
	if err := ValidateIngestURL(cfg.IngestURL, false); err != nil {
		t.Fatalf("default ingest URL must pass validation without DEV_MODE: %v", err)
	}
}

// INGEST_URL unset uses the default, set overrides it (local testing), and
// set-but-empty turns sending off.
func TestLoadIngestURL(t *testing.T) {
	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"unset":    {map[string]string{}, DefaultIngestURL},
		"override": {map[string]string{"INGEST_URL": "https://ingest.example.com/v1"}, "https://ingest.example.com/v1"},
		"empty":    {map[string]string{"INGEST_URL": ""}, ""},
		"blank":    {map[string]string{"INGEST_URL": "   "}, ""},
	} {
		c.env["CAPTAIN_DOMAIN"] = "nonprod.foo.onglueops.com"
		cfg, err := Load(env(c.env))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cfg.IngestURL != c.want {
			t.Errorf("%s: IngestURL = %q, want %q", name, cfg.IngestURL, c.want)
		}
	}
}

func TestLoadParsesEverything(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"CAPTAIN_DOMAIN":                 "nonprod.foo.onglueops.com",
		"INGEST_URL":                     "https://ingest.example.com/v1",
		"NAMESPACE_PATTERN":              " glueops-core-(argocd|vault).* ",
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
	if got := cfg.NamespacePattern.String(); got != "^(?:glueops-core-(argocd|vault).*)$" {
		t.Fatalf("namespace pattern not trimmed/anchored: %q", got)
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
		"bad ns pattern":         {"CAPTAIN_DOMAIN": "x.com", "NAMESPACE_PATTERN": "glueops-core("},
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
	if err != nil || cfg.HelmNamespace != DefaultHelmNamespace || cfg.NamespacePattern == nil || !cfg.NamespacePattern.MatchString("kube-system") {
		t.Fatalf("unset should default: %+v %v", cfg, err)
	}

	// Set but empty (or effectively empty): invalid_config.
	for name, m := range map[string]map[string]string{
		"NAMESPACE_PATTERN empty":      {"CAPTAIN_DOMAIN": "x.com", "NAMESPACE_PATTERN": ""},
		"NAMESPACE_PATTERN whitespace": {"CAPTAIN_DOMAIN": "x.com", "NAMESPACE_PATTERN": "   "},
		"HELM_NAMESPACE empty":         {"CAPTAIN_DOMAIN": "x.com", "HELM_NAMESPACE": ""},
		"HELM_NAMESPACE whitespace":    {"CAPTAIN_DOMAIN": "x.com", "HELM_NAMESPACE": " \t"},
	} {
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: expected invalid_config error", name)
		} else if !strings.Contains(err.Error(), "is set but") {
			t.Errorf("%s: unexpected error text %q", name, err)
		}
	}
}

func TestCompileNamespacePattern(t *testing.T) {
	// The default: kube-system and glueops-core*, whole-name only.
	def, err := CompileNamespacePattern(DefaultNamespacePattern)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"kube-system":              true,
		"glueops-core":             true,
		"glueops-core-monitoring":  true,
		"glueops-core-vault-extra": true,
		"glueops-corex":            false,
		"nonprod":                  false,
		"chisel-operator-system":   false,
		"default":                  false,
		"kube-public":              false,
		"aglueops-core":            false,
		"":                         false,
	} {
		if got := def.MatchString(name); got != want {
			t.Errorf("default pattern: %q matched=%v, want %v", name, got, want)
		}
	}

	// An unanchored user pattern is anchored by the agent, so it never
	// matches a longer name that merely contains it.
	user, err := CompileNamespacePattern("glueops-core")
	if err != nil {
		t.Fatal(err)
	}
	if !user.MatchString("glueops-core") || user.MatchString("aglueops-corez") || user.MatchString("glueops-core-monitoring") {
		t.Fatalf("user pattern must be anchored: %s", user)
	}

	// A pattern the user already anchored behaves identically, and a
	// partially anchored alternation still cannot match a longer name.
	for pattern, cases := range map[string]map[string]bool{
		"^kube-system$":         {"kube-system": true, "xkube-system": false, "kube-systemx": false},
		"^kube-system|default$": {"kube-system": true, "default": true, "xdefault": false, "kube-systemx": false},
	} {
		re, err := CompileNamespacePattern(pattern)
		if err != nil {
			t.Fatalf("%s: %v", pattern, err)
		}
		for name, want := range cases {
			if got := re.MatchString(name); got != want {
				t.Errorf("%s: %q matched=%v, want %v", pattern, name, got, want)
			}
		}
	}

	// Invalid syntax is an error that names the problem without echoing
	// the pattern.
	if _, err := CompileNamespacePattern("glueops-core-CANARY_PATTERN_TEXT("); err == nil {
		t.Fatal("expected an error for an unbalanced parenthesis")
	} else if strings.Contains(err.Error(), "CANARY_PATTERN_TEXT") || !strings.Contains(err.Error(), "regular expression") {
		t.Fatalf("unexpected error text %q", err)
	}
}

// A pattern with an unbalanced ")" must not close the anchoring group early.
// Pasted into ^(?:...)$ each of these compiles and matches every namespace,
// tenant ones included; they must be rejected as invalid instead.
func TestCompileNamespacePatternCannotEscapeAnchors(t *testing.T) {
	for _, pattern := range []string{
		`kube-system)|(?:.*`,
		`kube-system)|(.*`,
		`a)|.*|(b`,
	} {
		re, err := CompileNamespacePattern(pattern)
		if err == nil {
			t.Errorf("%s: accepted, compiled to %s (matches nonprod=%v)", pattern, re, re.MatchString("nonprod"))
			continue
		}
		if strings.Contains(err.Error(), pattern) || !strings.Contains(err.Error(), "regular expression") {
			t.Errorf("%s: unexpected error text %q", pattern, err)
		}
	}

	// Constructs that look like they could escape but cannot: they either stay
	// inside the group or fail to compile.
	for pattern, cases := range map[string]map[string]bool{
		`(?i)KUBE-SYSTEM`:      {"kube-system": true, "nonprod": false},
		`[)]|kube-system`:      {"kube-system": true, ")": true, "nonprod": false},
		`kube-system\)`:        {"kube-system)": true, "kube-system": false, "nonprod": false},
		`kube-system|\Q)|.*\E`: {"kube-system": true, ")|.*": true, "nonprod": false},
	} {
		re, err := CompileNamespacePattern(pattern)
		if err != nil {
			t.Fatalf("%s: %v", pattern, err)
		}
		for name, want := range cases {
			if got := re.MatchString(name); got != want {
				t.Errorf("%s: %q matched=%v, want %v", pattern, name, got, want)
			}
		}
	}
	if _, err := CompileNamespacePattern(`kube-system|\Q`); err == nil {
		t.Error(`kube-system|\Q: an unterminated \Q quotes the wrapper's closing group and must be rejected`)
	}
}

// withUserinfo attaches canary credentials at runtime so the source never
// contains a credential-bearing URL literal.
func withUserinfo(base string, withPassword bool) string {
	u, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	if withPassword {
		u.User = url.UserPassword("canary-user", "CANARY_USERINFO")
	} else {
		u.User = url.User("canary-user")
	}
	return u.String()
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
		{withUserinfo("https://ingest.example.com/v1", true), false, false},
		{withUserinfo("https://ingest.example.com/v1", false), false, false},
		{withUserinfo("http://localhost:8080/", true), true, false},
	}
	for _, c := range cases {
		err := ValidateIngestURL(c.url, c.dev)
		if (err == nil) != c.want {
			t.Errorf("ValidateIngestURL(%q, dev=%v) = %v, want ok=%v", c.url, c.dev, err, c.want)
		}
	}
}
