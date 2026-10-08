package collect

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	payload "github.com/glueops/inventory-agent/internal/schema"
)

func node(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"node.kubernetes.io/instance-type": "m5.large"}},
		Spec:       corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-0123456789abcdef0"},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
			KubeletVersion:          "v1.35.8+k3s1",
			OSImage:                 "K3s v1.35.8+k3s1",
			OperatingSystem:         "linux",
			Architecture:            "amd64",
			KernelVersion:           "6.12.107+deb13-amd64",
			ContainerRuntimeVersion: "containerd://2.2.7-k3s1",
			KubeProxyVersion:        "v1.35.8+k3s1",
			MachineID:               "machine-id-should-not-leak",
			BootID:                  "boot-id-should-not-leak",
		}},
	}
}

func clusterClient(objs ...runtime.Object) *fake.Clientset {
	client := fake.NewClientset(objs...)
	client.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{
		Major: "1", Minor: "35", GitVersion: "v1.35.8+k3s1", Platform: "linux/amd64",
	}
	return client
}

func TestClusterCollectsVersionAndNodes(t *testing.T) {
	client := clusterClient(node("k3d-captain-server-0"), node("k3d-captain-agent-1"), node("k3d-captain-agent-0"))
	log, _ := testLogger()
	section := Cluster(context.Background(), client, log)
	if section.Status != payload.StatusOK || section.Data == nil {
		t.Fatalf("unexpected: %+v", section)
	}
	d := section.Data
	if d.K8sVersion != "v1.35.8+k3s1" || d.K8sMajor != "1" || d.K8sMinor != "35" || d.Platform != "linux/amd64" {
		t.Fatalf("unexpected version: %+v", d)
	}
	if len(d.Nodes) != 3 || d.Nodes[0].NodeName != "k3d-captain-agent-0" || d.Nodes[2].NodeName != "k3d-captain-server-0" {
		t.Fatalf("nodes not sorted/complete: %+v", d.Nodes)
	}
	raw, _ := json.Marshal(section)
	for _, forbidden := range []string{"should-not-leak", "providerID", "aws:///", "m5.large", "kubeProxyVersion", "kube_proxy"} {
		if contains(string(raw), forbidden) {
			t.Errorf("cluster payload leaks %q", forbidden)
		}
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestClusterNodesForbidden(t *testing.T) {
	client := clusterClient()
	client.PrependReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("denied"))
	})
	log, _ := testLogger()
	section := Cluster(context.Background(), client, log)
	if section.Status != payload.StatusError || section.Error != payload.ErrRBACDenied || section.Data != nil {
		t.Fatalf("expected rbac_denied, got %+v", section)
	}
}

func TestClusterNilClientIsAPIUnavailable(t *testing.T) {
	log, _ := testLogger()
	section := Cluster(context.Background(), nil, log)
	if section.Status != payload.StatusError || section.Error != payload.ErrAPIUnavailable {
		t.Fatalf("expected api_unavailable, got %+v", section)
	}
}

func TestClusterUID(t *testing.T) {
	client := clusterClient(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("7897eb25-7a75-4903-b0f2-b8a849977428")}})
	uid, err := ClusterUID(context.Background(), client)
	if err != nil || uid != "7897eb25-7a75-4903-b0f2-b8a849977428" {
		t.Fatalf("got %q, %v", uid, err)
	}
	if _, err := ClusterUID(context.Background(), clusterClient()); !apierrors.IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestClassify(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}
	cases := []struct {
		err  error
		want string
	}{
		{apierrors.NewForbidden(gr, "x", errors.New("no")), payload.ErrRBACDenied},
		{apierrors.NewUnauthorized("no"), payload.ErrRBACDenied},
		{apierrors.NewServiceUnavailable("down"), payload.ErrAPIUnavailable},
		{apierrors.NewInternalError(errors.New("boom")), payload.ErrAPIUnavailable},
		{apierrors.NewTimeoutError("slow", 1), payload.ErrAPIUnavailable},
		{apierrors.NewServerTimeout(gr, "list", 1), payload.ErrAPIUnavailable},
		{apierrors.NewTooManyRequests("slow down", 1), payload.ErrAPIUnavailable},
		{apierrors.NewGenericServerResponse(502, "list", gr, "", "bad gateway", 0, true), payload.ErrAPIUnavailable},
		{&url.Error{Op: "Get", URL: "https://10.0.0.1", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, payload.ErrAPIUnavailable},
		{context.DeadlineExceeded, payload.ErrAPIUnavailable},
		{errNoClient, payload.ErrAPIUnavailable},
		{&DecodeError{Secret: "s", Stage: "gzip", Err: errors.New("x")}, payload.ErrDecodeFailed},
		{apierrors.NewNotFound(gr, "x"), payload.ErrInternal},
		{apierrors.NewBadRequest("bad"), payload.ErrInternal},
		{errors.New("something else"), payload.ErrInternal},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("Classify(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}

func TestFormatTime(t *testing.T) {
	if got := FormatTime(parseHelmTime("2026-09-29T17:53:59.278912345+02:00")); got != "2026-09-29T15:53:59.278Z" {
		t.Fatalf("got %s", got)
	}
	if got := FormatTime(parseHelmTime("2026-09-29T15:53:59Z")); got != "2026-09-29T15:53:59.000Z" {
		t.Fatalf("got %s", got)
	}
	if formatTimePtr(parseHelmTime("garbage")) != nil {
		t.Fatal("expected nil for unparseable time")
	}
}
