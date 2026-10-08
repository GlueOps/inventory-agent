package collect

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	payload "github.com/glueops/inventory-agent/internal/schema"
)

func TestEnvelopeAssemblesAllSections(t *testing.T) {
	objs := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
		node("n1"),
		helmSecret(t, "glueops-core", "argocd", 1, helmReleaseJSON("argocd", "glueops-core", 1, "deployed", "argo-cd", "10.2.2", "v3.4.6",
			"2026-09-29T15:53:59.278Z", "2026-09-29T15:53:59.278Z")),
		otherSecret("glueops-core"),
	}
	for _, p := range k3dAndEKSPods() {
		objs = append(objs, p)
	}
	client := clusterClient(objs...)
	log, logs := testLogger()
	now := time.Date(2026, 10, 1, 5, 0, 0, 412_000_000, time.UTC)

	env := Envelope(context.Background(), client, Params{
		CaptainDomain: "nonprod.foobar.onglueops.com", PlatformChartVersion: "0.80.2", CollectorVersion: "v0.1.0",
		RunID: "20261001050000_3f9c2a7e", HelmNamespace: "glueops-core",
		PodNamespaces: []string{"kube-system", "glueops-core"}, MaxPodRows: 5000,
	}, now, log)

	if env.SchemaVersion != 1 || env.CaptainDomain != "nonprod.foobar.onglueops.com" || *env.ClusterUID != "uid-123" ||
		env.RunID != "20261001050000_3f9c2a7e" || env.CollectedAt != "2026-10-01T05:00:00.412Z" ||
		env.CollectorVersion != "v0.1.0" || env.PlatformChartVersion != "0.80.2" {
		t.Fatalf("unexpected run-level fields: %+v", env)
	}
	if env.Datasets.Cluster.Status != payload.StatusOK || env.Datasets.HelmReleases.Status != payload.StatusOK || env.Datasets.PodImages.Status != payload.StatusOK {
		t.Fatalf("expected all sections ok: %+v", env.Datasets)
	}
	raw, _ := json.Marshal(env)
	assertNoMarkers(t, "envelope", string(raw))
	assertNoMarkers(t, "logs", logs.String())
}

func TestEnvelopeWithNilClient(t *testing.T) {
	log, logs := testLogger()
	env := Envelope(context.Background(), nil, Params{CaptainDomain: "x", PodNamespaces: []string{"kube-system"}}, time.Now(), log)
	if env.ClusterUID != nil {
		t.Fatal("expected nil cluster_uid")
	}
	for name, st := range map[string][2]string{
		"cluster":       {env.Datasets.Cluster.Status, env.Datasets.Cluster.Error},
		"helm_releases": {env.Datasets.HelmReleases.Status, env.Datasets.HelmReleases.Error},
		"pod_images":    {env.Datasets.PodImages.Status, env.Datasets.PodImages.Error},
	} {
		if st[0] != payload.StatusError || st[1] != payload.ErrAPIUnavailable {
			t.Errorf("%s: got %v, want error/api_unavailable", name, st)
		}
	}
	if !strings.Contains(logs.String(), `"reason":"cluster_uid_unavailable"`) {
		t.Fatalf("expected cluster_uid_unavailable log, got %s", logs.String())
	}
}

func TestEnvelopeSectionPanicIsIsolated(t *testing.T) {
	client := clusterClient(node("n1"))
	client.PrependReactor("list", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		panic("boom in secrets")
	})
	log, logs := testLogger()
	env := Envelope(context.Background(), client, Params{CaptainDomain: "x", HelmNamespace: "glueops-core", PodNamespaces: []string{"kube-system"}, MaxPodRows: 10}, time.Now(), log)
	if env.Datasets.HelmReleases.Status != payload.StatusError || env.Datasets.HelmReleases.Error != payload.ErrInternal {
		t.Fatalf("expected internal_error for helm, got %+v", env.Datasets.HelmReleases)
	}
	if env.Datasets.Cluster.Status != payload.StatusOK || env.Datasets.PodImages.Status != payload.StatusOK {
		t.Fatalf("other sections must survive: %+v", env.Datasets)
	}
	if !strings.Contains(logs.String(), "section panicked") {
		t.Fatal("expected panic log")
	}
}
