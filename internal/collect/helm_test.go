package collect

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	gort "runtime"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	payload "github.com/glueops/inventory-agent/internal/schema"
)

func TestHelmReleasesExtractsOnlyMetadata(t *testing.T) {
	ns := "glueops-core"
	client := fake.NewClientset(
		helmSecret(t, ns, "argocd", 1, helmReleaseJSON("argocd", ns, 1, "deployed", "argo-cd", "10.2.2", "v3.4.6",
			"2026-09-29T15:53:59.278912345Z", "2026-09-29T15:53:59.278912345Z")),
		helmSecret(t, ns, "glueops-platform", 1, helmReleaseJSON("glueops-platform", ns, 1, "superseded", "glueops-platform", "0.79.0", "v0.1.0",
			"2026-09-29T15:57:21.815Z", "2026-09-29T15:57:21.815Z")),
		helmSecret(t, ns, "glueops-platform", 2, helmReleaseJSON("glueops-platform", ns, 2, "failed", "glueops-platform", "0.80.2", "v0.1.0",
			"2026-09-29T15:57:21.815Z", "2026-10-01T04:00:00Z")),
		otherSecret(ns),
		// A Helm secret in another namespace must not be picked up.
		helmSecret(t, "other", "elsewhere", 1, helmReleaseJSON("elsewhere", "other", 1, "deployed", "x", "1", "1", "", "")),
	)
	log, logs := testLogger()

	section, _ := HelmReleases(context.Background(), client, ns, log)
	if section.Status != payload.StatusOK || section.Error != "" {
		t.Fatalf("unexpected section: %+v", section)
	}
	first := "2026-09-29T15:53:59.278Z"
	pFirst := "2026-09-29T15:57:21.815Z"
	pLast := "2026-10-01T04:00:00.000Z"
	want := []payload.HelmRelease{
		{ReleaseName: "argocd", Namespace: ns, ChartName: "argo-cd", ChartVersion: "10.2.2", AppVersion: "v3.4.6",
			Revision: 1, Status: "deployed", FirstDeployed: &first, LastDeployed: &first},
		{ReleaseName: "glueops-platform", Namespace: ns, ChartName: "glueops-platform", ChartVersion: "0.80.2", AppVersion: "v0.1.0",
			Revision: 2, Status: "failed", FirstDeployed: &pFirst, LastDeployed: &pLast},
	}
	if !reflect.DeepEqual(section.Data, want) {
		got, _ := json.MarshalIndent(section.Data, "", "  ")
		t.Fatalf("unexpected releases:\n%s", got)
	}

	// Exactly nine keys per release on the wire, and no sensitive content
	// anywhere in the payload or logs.
	raw, _ := json.Marshal(section)
	var decoded struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(raw, &decoded)
	wantKeys := []string{"app_version", "chart_name", "chart_version", "first_deployed", "last_deployed", "namespace", "release_name", "revision", "status"}
	for _, rel := range decoded.Data {
		var keys []string
		for k := range rel {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, wantKeys) {
			t.Errorf("release keys %v != %v", keys, wantKeys)
		}
	}
	assertNoMarkers(t, "helm payload", string(raw))
	assertNoMarkers(t, "logs", logs.String())
}

func TestHelmReleasesEmptyIsOK(t *testing.T) {
	client := fake.NewClientset(otherSecret("glueops-core"))
	log, _ := testLogger()
	section, _ := HelmReleases(context.Background(), client, "glueops-core", log)
	if section.Status != payload.StatusOK || section.Data == nil || len(section.Data) != 0 {
		t.Fatalf("expected ok with empty list, got %+v", section)
	}
	raw, _ := json.Marshal(section)
	if string(raw) != `{"schema_version":1,"status":"ok","data":[]}` {
		t.Fatalf("unexpected wire form: %s", raw)
	}
}

func TestHelmReleasesRBACDenied(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("denied"))
	})
	log, logs := testLogger()
	section, _ := HelmReleases(context.Background(), client, "glueops-core", log)
	if section.Status != payload.StatusError || section.Error != payload.ErrRBACDenied || section.Data != nil {
		t.Fatalf("expected rbac_denied, got %+v", section)
	}
	raw, _ := json.Marshal(section)
	if string(raw) != `{"schema_version":1,"status":"error","error":"rbac_denied","data":null}` {
		t.Fatalf("unexpected wire form: %s", raw)
	}
	if logs.Len() == 0 {
		t.Fatal("expected a warning log")
	}
}

func TestHelmDecodeFailureIsDecodeFailedAndSafeToLog(t *testing.T) {
	ns := "glueops-core"
	bad := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.bad.v1", Namespace: ns, Labels: map[string]string{"owner": "helm"}},
		Type:       helmSecretType,
		Data:       map[string][]byte{"release": []byte("!!! not base64 " + markerValues)},
	}
	client := fake.NewClientset(bad)
	log, logs := testLogger()
	section, skipped := HelmReleases(context.Background(), client, ns, log)
	if section.Status != payload.StatusError || section.Error != payload.ErrDecodeFailed || skipped != 1 {
		t.Fatalf("expected decode_failed when every secret is undecodable, got %+v skipped=%d", section, skipped)
	}
	assertNoMarkers(t, "logs", logs.String())
}

// TestHelmSkipsUndecodableSecretKeepsOthers: one garbage Secret next to a
// good one must not blind the whole section.
func TestHelmSkipsUndecodableSecretKeepsOthers(t *testing.T) {
	ns := "glueops-core"
	bad := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.bad.v1", Namespace: ns, Labels: map[string]string{"owner": "helm"}},
		Type:       helmSecretType,
		Data:       map[string][]byte{"release": []byte("!!! not base64 " + markerValues)},
	}
	good := helmSecret(t, ns, "argocd", 1, helmReleaseJSON("argocd", ns, 1, "deployed", "argo-cd", "10.2.2", "v3.4.6",
		"2026-09-29T15:53:59.278Z", "2026-09-29T15:53:59.278Z"))
	client := fake.NewClientset(bad, good)
	log, logs := testLogger()

	section, skipped := HelmReleases(context.Background(), client, ns, log)
	if section.Status != payload.StatusOK || section.Error != "" {
		t.Fatalf("expected ok, got %+v", section)
	}
	if len(section.Data) != 1 || section.Data[0].ReleaseName != "argocd" {
		t.Fatalf("expected the good release only, got %+v", section.Data)
	}
	if skipped != 1 {
		t.Fatalf("expected skipped=1, got %d", skipped)
	}
	out := logs.String()
	if !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, `"reason":"decode_failed"`) ||
		!strings.Contains(out, `"secret":"sh.helm.release.v1.bad.v1"`) || !strings.Contains(out, `"stage":"base64"`) {
		t.Fatalf("expected a decode_failed warning naming the secret and stage:\n%s", out)
	}
	assertNoMarkers(t, "logs", out)
}

func TestHelmDecodeAcceptsUncompressedRelease(t *testing.T) {
	rel := helmReleaseJSON("plain", "glueops-core", 3, "deployed", "c", "1.2.3", "4.5.6", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.plain.v3", Namespace: "glueops-core",
			Labels: map[string]string{"owner": "helm", "version": "3"}},
		Type: helmSecretType,
		Data: map[string][]byte{"release": encodeRelease(t, rel, false)},
	}
	got, err := decodeReleaseSecret(sec)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReleaseName != "plain" || got.Revision != 3 || got.ChartVersion != "1.2.3" || *got.FirstDeployed != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("unexpected release: %+v", got)
	}
}

func TestHelmSkipsNonHelmTypedSecretsEvenWithLabel(t *testing.T) {
	// A Secret carrying owner=helm but a different type must never be decoded.
	sec := otherSecret("glueops-core")
	sec.Labels = map[string]string{"owner": "helm"}
	client := fake.NewClientset(sec)
	log, _ := testLogger()
	section, _ := HelmReleases(context.Background(), client, "glueops-core", log)
	if section.Status != payload.StatusOK || len(section.Data) != 0 {
		t.Fatalf("expected ok/empty, got %+v", section)
	}
}

// TestHelmGzipBombIsBoundedAndSpecific feeds a release Secret whose gzip
// expands to more than maxReleaseBytes: decoding must stop with the
// "release too large" stage and allocate well under 64 MiB.
func TestHelmGzipBombIsBoundedAndSpecific(t *testing.T) {
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	zeros := make([]byte, 1<<20)
	for i := 0; i < (maxReleaseBytes>>20)+1; i++ { // 17 MiB of zeros
		if _, err := zw.Write(zeros); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.bomb.v1", Namespace: "glueops-core", Labels: map[string]string{"owner": "helm"}},
		Type:       helmSecretType,
		Data:       map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(zipped.Bytes()))},
	}

	gort.GC()
	var before, after gort.MemStats
	gort.ReadMemStats(&before)
	_, err := decodeReleaseSecret(sec)
	gort.ReadMemStats(&after)

	var de *DecodeError
	if !errors.As(err, &de) || de.Stage != "release too large" {
		t.Fatalf("expected DecodeError with stage %q, got %v", "release too large", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Fatalf("decoding allocated %d MiB, want well under 64 MiB", grew>>20)
	}
}

func TestReadBounded(t *testing.T) {
	data, tooLarge, err := readBounded(strings.NewReader("hello"), 5)
	if err != nil || tooLarge || string(data) != "hello" {
		t.Fatalf("exact size: %q %v %v", data, tooLarge, err)
	}
	_, tooLarge, err = readBounded(strings.NewReader("hello!"), 5)
	if err != nil || !tooLarge {
		t.Fatalf("one over: %v %v", tooLarge, err)
	}
	big := strings.Repeat("x", 300<<10)
	data, tooLarge, err = readBounded(strings.NewReader(big), 1<<20)
	if err != nil || tooLarge || string(data) != big {
		t.Fatalf("multi-grow read mismatch: len=%d tooLarge=%v err=%v", len(data), tooLarge, err)
	}
}
