package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
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

func podsClient(pods ...*corev1.Pod) *fake.Clientset {
	objs := make([]runtime.Object, 0, len(pods))
	for _, p := range pods {
		objs = append(objs, p)
	}
	return fake.NewClientset(objs...)
}

type rowKey struct{ pod, container string }

func indexRows(rows []payload.PodImage) map[rowKey]payload.PodImage {
	m := map[rowKey]payload.PodImage{}
	for _, r := range rows {
		m[rowKey{r.PodName, r.ContainerName}] = r
	}
	return m
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestPodImagesOwnershipAndImages(t *testing.T) {
	client := podsClient(k3dAndEKSPods()...)
	log, logs := testLogger()
	section := PodImages(context.Background(), client, PodImagesParams{
		Namespaces: []string{"kube-system", "glueops-core"}, MaxRows: 5000,
	}, log)
	if section.Status != payload.StatusOK || section.Truncated {
		t.Fatalf("unexpected section: %+v", section)
	}
	if !reflect.DeepEqual(section.NamespacesRequested, []string{"kube-system", "glueops-core"}) || len(section.NamespacesDenied) != 0 {
		t.Fatalf("unexpected namespaces: %+v / %+v", section.NamespacesRequested, section.NamespacesDenied)
	}
	// 1 + 1 + 1 + 3 + 1 + 1 + 1 + 1 + 2 + 1 containers (ephemeral excluded)
	if len(section.Data) != 13 {
		t.Fatalf("expected 13 rows, got %d", len(section.Data))
	}
	rows := indexRows(section.Data)

	cases := []struct {
		pod, container             string
		ownerKind, ownerName       string
		workloadKind, workloadName string
		ctype, phase, node         string
		imageID, digest            string
	}{
		{"coredns-c5fdd76cf-bb6t2", "coredns", "ReplicaSet", "coredns-c5fdd76cf", "Deployment", "coredns", "app", "Running", "k3d-captain-agent-4",
			"docker.io/rancher/mirrored-coredns-coredns@" + digestCoreDNS, digestCoreDNS},
		// Bare containerd imageID: image_id kept, image_digest null (config digest, not manifest digest).
		{"aws-node-x7k2p", "aws-node", "DaemonSet", "aws-node", "DaemonSet", "aws-node", "app", "Running", "ip-10-0-1-23.ec2.internal",
			digestNode, "<nil>"},
		{"ebs-csi-node-4jk9z", "init-dir", "DaemonSet", "ebs-csi-node", "DaemonSet", "ebs-csi-node", "init", "Running", "ip-10-0-1-23.ec2.internal",
			"public.ecr.aws/eks-distro/kubernetes-csi/livenessprobe@" + digestCSI, digestCSI},
		{"ebs-csi-node-4jk9z", "node-driver-registrar", "DaemonSet", "ebs-csi-node", "DaemonSet", "ebs-csi-node", "app", "Running", "ip-10-0-1-23.ec2.internal",
			"docker-pullable://public.ecr.aws/eks-distro/kubernetes-csi/node-driver-registrar@" + digestNode, digestNode},
		{"loki-0", "loki", "StatefulSet", "loki", "StatefulSet", "loki", "app", "Running", "k3d-captain-agent-1",
			"docker.io/grafana/loki@" + digestCSI, digestCSI},
		{"one-off-migrate-abcde", "migrate", "Job", "one-off-migrate", "Job", "one-off-migrate", "app", "Succeeded", "k3d-captain-agent-2",
			"ghcr.io/glueops/migrate@" + digestCSI, digestCSI},
		{"backups-and-exports-29837460-k2x9p", "backup", "Job", "backups-and-exports-29837460", "CronJob", "backups-and-exports", "app", "Succeeded", "k3d-captain-agent-2",
			"ghcr.io/glueops/backups@" + digestCSI, digestCSI},
		{"debug-shell", "shell", "<nil>", "<nil>", "Pod", "debug-shell", "app", "Running", "k3d-captain-agent-3",
			"docker.io/library/busybox@" + digestNode, digestNode},
		{"pending-7d9f8-zzzzz", "wait", "ReplicaSet", "pending-7d9f8", "Deployment", "pending", "init", "Pending", "<nil>", "<nil>", "<nil>"},
		{"pending-7d9f8-zzzzz", "app", "ReplicaSet", "pending-7d9f8", "Deployment", "pending", "app", "Pending", "<nil>", "<nil>", "<nil>"},
		{"bare-rs-abcde", "app", "ReplicaSet", "bare-rs", "ReplicaSet", "bare-rs", "app", "Running", "k3d-captain-agent-3",
			"docker://legacy-opaque-id-without-digest", "<nil>"},
	}
	for _, c := range cases {
		r, ok := rows[rowKey{c.pod, c.container}]
		if !ok {
			t.Errorf("missing row %s/%s", c.pod, c.container)
			continue
		}
		got := []string{deref(r.OwnerKind), deref(r.OwnerName), r.WorkloadKind, r.WorkloadName, r.ContainerType, r.PodPhase, deref(r.NodeName), deref(r.ImageID), deref(r.ImageDigest)}
		want := []string{c.ownerKind, c.ownerName, c.workloadKind, c.workloadName, c.ctype, c.phase, c.node, c.imageID, c.digest}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s/%s:\n got %v\nwant %v", c.pod, c.container, got, want)
		}
	}
	for _, r := range section.Data {
		if r.ContainerName == "debugger" {
			t.Error("ephemeral container must be excluded")
		}
	}

	raw, _ := json.Marshal(section)
	assertNoMarkers(t, "pod_images payload", string(raw))
	assertNoMarkers(t, "logs", logs.String())
	for _, forbidden := range []string{"env", "args", "command", "volumes", "resources"} {
		if containsJSONKey(t, raw, forbidden) {
			t.Errorf("payload contains forbidden key %q", forbidden)
		}
	}
}

func containsJSONKey(t *testing.T, raw []byte, key string) bool {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	var walk func(any) bool
	walk = func(x any) bool {
		switch xx := x.(type) {
		case map[string]any:
			for k, val := range xx {
				if k == key || walk(val) {
					return true
				}
			}
		case []any:
			for _, val := range xx {
				if walk(val) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}

func TestPodImagesTruncation(t *testing.T) {
	var pods []*corev1.Pod
	for i := 0; i < 50; i++ {
		pods = append(pods, podFixture{
			ns: "glueops-core", name: fmt.Sprintf("big-%03d", i), phase: "Running", node: "n",
			owner: ownerRef("DaemonSet", "big"), apps: []string{"a=img:1", "b=img:2"},
		}.build())
	}
	client := podsClient(pods...)
	log, _ := testLogger()
	section := PodImages(context.Background(), client, PodImagesParams{Namespaces: []string{"glueops-core"}, MaxRows: 25}, log)
	if section.Status != payload.StatusOK {
		t.Fatalf("expected ok, got %+v", section)
	}
	if !section.Truncated || len(section.Data) != 25 {
		t.Fatalf("expected truncated=true with 25 rows, got truncated=%v rows=%d", section.Truncated, len(section.Data))
	}
	// Exactly at the cap is not truncation.
	section = PodImages(context.Background(), client, PodImagesParams{Namespaces: []string{"glueops-core"}, MaxRows: 100}, log)
	if section.Truncated || len(section.Data) != 100 {
		t.Fatalf("expected untruncated 100 rows, got truncated=%v rows=%d", section.Truncated, len(section.Data))
	}
}

func TestPodImagesPartialDenialStaysOK(t *testing.T) {
	client := podsClient(k3dAndEKSPods()...)
	client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "glueops-core-denied" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("denied"))
		}
		return false, nil, nil
	})
	log, _ := testLogger()
	section := PodImages(context.Background(), client, PodImagesParams{
		Namespaces: []string{"kube-system", "glueops-core-denied", "glueops-core-does-not-exist", "glueops-core"}, MaxRows: 5000,
	}, log)
	if section.Status != payload.StatusOK {
		t.Fatalf("expected ok, got %+v", section)
	}
	// Only RBAC refusals are "denied"; a namespace that does not exist lists
	// as empty and is not recorded anywhere.
	if !reflect.DeepEqual(section.NamespacesDenied, []string{"glueops-core-denied"}) {
		t.Fatalf("unexpected denied: %v", section.NamespacesDenied)
	}
	if len(section.Data) != 13 {
		t.Fatalf("expected rows from readable namespaces, got %d", len(section.Data))
	}
}

func TestPodImagesAllNamespacesDeniedIsRBACError(t *testing.T) {
	client := podsClient(k3dAndEKSPods()...)
	client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("denied"))
	})
	log, logs := testLogger()
	requested := []string{"kube-system", "glueops-core"}
	section := PodImages(context.Background(), client, PodImagesParams{Namespaces: requested, MaxRows: 5000}, log)
	if section.Status != payload.StatusError || section.Error != payload.ErrRBACDenied || section.Data != nil {
		t.Fatalf("expected error/rbac_denied with null data, got %+v", section)
	}
	if !reflect.DeepEqual(section.NamespacesDenied, requested) || !reflect.DeepEqual(section.NamespacesRequested, requested) {
		t.Fatalf("namespace lists must be kept: %+v", section)
	}
	raw, _ := json.Marshal(section)
	want := `{"schema_version":1,"status":"error","error":"rbac_denied","namespaces_requested":["kube-system","glueops-core"],"namespaces_denied":["kube-system","glueops-core"],"truncated":false,"data":null}`
	if string(raw) != want {
		t.Fatalf("unexpected wire form:\n%s", raw)
	}
	if !strings.Contains(logs.String(), `"reason":"rbac_denied"`) {
		t.Fatal("expected rbac_denied in logs")
	}
}

// pagingLister serves pods in fixed-size pages with Continue tokens and
// records every call so the test can assert Limit/Continue handling.
type pagingLister struct {
	pods     map[string][]corev1.Pod
	pageSize int
	calls    []metav1.ListOptions
}

func (l *pagingLister) list(_ context.Context, ns string, opts metav1.ListOptions) (*corev1.PodList, error) {
	l.calls = append(l.calls, opts)
	all := l.pods[ns]
	start := 0
	if opts.Continue != "" { // token format: "<ns>:<next index>"
		start, _ = strconv.Atoi(opts.Continue[strings.LastIndex(opts.Continue, ":")+1:])
	}
	end := start + l.pageSize
	if int(opts.Limit) > 0 && int(opts.Limit) < l.pageSize {
		end = start + int(opts.Limit)
	}
	if end > len(all) {
		end = len(all)
	}
	page := &corev1.PodList{Items: append([]corev1.Pod{}, all[start:end]...)}
	if end < len(all) {
		page.Continue = fmt.Sprintf("%s:%d", ns, end)
	}
	return page, nil
}

func makePods(ns string, n int) []corev1.Pod {
	out := make([]corev1.Pod, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, *podFixture{ns: ns, name: fmt.Sprintf("p-%04d", i), phase: "Running", node: "n",
			owner: ownerRef("DaemonSet", "d"), apps: []string{"a=img:1"}}.build())
	}
	return out
}

func TestPodImagesPaginatesWithContinue(t *testing.T) {
	l := &pagingLister{pageSize: 3, pods: map[string][]corev1.Pod{"a": makePods("a", 7), "b": makePods("b", 2)}}
	log, _ := testLogger()
	section := collectPodImages(context.Background(), l.list, PodImagesParams{Namespaces: []string{"a", "b"}, MaxRows: 5000}, log)
	if section.Status != payload.StatusOK || section.Truncated || len(section.Data) != 9 {
		t.Fatalf("expected 9 rows across pages, got %+v (rows=%d)", section, len(section.Data))
	}
	// a: 3 pages (3,3,1); b: 1 page.
	if len(l.calls) != 4 {
		t.Fatalf("expected 4 list calls, got %d: %+v", len(l.calls), l.calls)
	}
	for i, c := range l.calls {
		if c.Limit != podPageSize {
			t.Errorf("call %d: Limit=%d, want %d", i, c.Limit, podPageSize)
		}
	}
	if l.calls[0].Continue != "" || l.calls[1].Continue != "a:3" || l.calls[2].Continue != "a:6" || l.calls[3].Continue != "" {
		t.Fatalf("unexpected Continue sequence: %+v", l.calls)
	}
	// Rows must come from every page, not just the first.
	if section.Data[0].PodName != "p-0000" || section.Data[6].PodName != "p-0006" || section.Data[8].Namespace != "b" {
		t.Fatalf("rows missing from later pages: %+v", section.Data)
	}
}

func TestPodImagesStopsPagingAtRowCap(t *testing.T) {
	l := &pagingLister{pageSize: 3, pods: map[string][]corev1.Pod{"a": makePods("a", 30), "b": makePods("b", 30)}}
	log, _ := testLogger()
	section := collectPodImages(context.Background(), l.list, PodImagesParams{Namespaces: []string{"a", "b"}, MaxRows: 4}, log)
	if !section.Truncated || len(section.Data) != 4 {
		t.Fatalf("expected truncated at 4 rows, got truncated=%v rows=%d", section.Truncated, len(section.Data))
	}
	// Page 1 fills 3 rows, page 2 overflows at the 5th row; nothing further.
	if len(l.calls) != 2 {
		t.Fatalf("expected paging to stop after the cap, got %d calls: %+v", len(l.calls), l.calls)
	}
}

func TestPodImagesAPIUnavailableFailsSection(t *testing.T) {
	client := podsClient()
	client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd down")
	})
	log, _ := testLogger()
	section := PodImages(context.Background(), client, PodImagesParams{Namespaces: []string{"kube-system"}, MaxRows: 10}, log)
	if section.Status != payload.StatusError || section.Error != payload.ErrAPIUnavailable || section.Data != nil {
		t.Fatalf("expected api_unavailable, got %+v", section)
	}
	raw, _ := json.Marshal(section)
	want := `{"schema_version":1,"status":"error","error":"api_unavailable","namespaces_requested":["kube-system"],"namespaces_denied":[],"truncated":false,"data":null}`
	if string(raw) != want {
		t.Fatalf("unexpected wire form:\n%s", raw)
	}
}

func TestPodImagesEmptyNamespaceIsOK(t *testing.T) {
	client := podsClient()
	log, _ := testLogger()
	section := PodImages(context.Background(), client, PodImagesParams{Namespaces: []string{"kube-system"}, MaxRows: 10}, log)
	raw, _ := json.Marshal(section)
	want := `{"schema_version":1,"status":"ok","namespaces_requested":["kube-system"],"namespaces_denied":[],"truncated":false,"data":[]}`
	if string(raw) != want {
		t.Fatalf("unexpected wire form:\n%s", raw)
	}
}

func TestDigest(t *testing.T) {
	hex := "900f9c109f7a33545d3c811516e8376df9019147b750f5ce3e254468769176ea"
	cases := map[string]string{
		"docker.io/rancher/coredns@sha256:" + hex: "sha256:" + hex,
		"sha256:" + hex:                          "<nil>", // bare config digest is not a manifest digest
		"docker-pullable://repo/x@sha256:" + hex: "sha256:" + hex,
		"@sha256:" + hex:                         "sha256:" + hex,
		"":                                       "<nil>",
		"docker://abcdef":                        "<nil>",
		"sha256:short":                           "<nil>",
		"sha256:" + hex[:63] + "G":               "<nil>",
		"repo@sha512:" + hex:                     "<nil>",
		"repo:tag":                               "<nil>",
	}
	for in, want := range cases {
		if got := deref(Digest(in)); got != want {
			t.Errorf("Digest(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeriveOwnershipEdgeCases(t *testing.T) {
	// Owner kinds outside the known set pass through (e.g. static pods owned by a Node).
	pod := podFixture{ns: "kube-system", name: "kube-apiserver-master-0", phase: "Running", node: "master-0",
		owner: &metav1.OwnerReference{APIVersion: "v1", Kind: "Node", Name: "master-0"}, apps: []string{"a=i"}}.build()
	ok, on, wk, wn := deriveOwnership(pod)
	if *ok != "Node" || *on != "master-0" || wk != "Node" || wn != "master-0" {
		t.Fatalf("unexpected: %s %s %s %s", *ok, *on, wk, wn)
	}
	// Job name with a 10-digit suffix is also a CronJob; 7 digits is not.
	for name, want := range map[string][2]string{
		"nightly-1700000000": {"CronJob", "nightly"},
		"nightly-1234567":    {"Job", "nightly-1234567"},
		"29837460":           {"Job", "29837460"},
	} {
		pod := podFixture{ns: "x", name: name + "-abc", owner: ownerRef("Job", name), apps: []string{"a=i"}}.build()
		_, _, wk, wn := deriveOwnership(pod)
		if wk != want[0] || wn != want[1] {
			t.Errorf("%s: got %s/%s want %s/%s", name, wk, wn, want[0], want[1])
		}
	}
	// The controller reference wins even when it is not first.
	pod = podFixture{ns: "x", name: "web-abc-xyz", labels: map[string]string{"pod-template-hash": "abc"}, apps: []string{"a=i"}}.build()
	pod.OwnerReferences = []metav1.OwnerReference{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "x"},
		*ownerRef("ReplicaSet", "web-abc"),
	}
	ok, on, wk, wn = deriveOwnership(pod)
	if *ok != "ReplicaSet" || *on != "web-abc" || wk != "Deployment" || wn != "web" {
		t.Fatalf("controller reference not preferred: %s %s %s %s", *ok, *on, wk, wn)
	}
	// With no reference marked controller the first one is used.
	pod.OwnerReferences[1].Controller = nil
	ok, on, wk, wn = deriveOwnership(pod)
	if *ok != "ConfigMap" || *on != "x" || wk != "ConfigMap" || wn != "x" {
		t.Fatalf("expected first reference as fallback: %s %s %s %s", *ok, *on, wk, wn)
	}
	// ReplicaSet whose name does not end in the hash is left alone.
	pod = podFixture{ns: "x", name: "p", owner: ownerRef("ReplicaSet", "web-abc"), labels: map[string]string{"pod-template-hash": "zzz"}, apps: []string{"a=i"}}.build()
	_, _, wk, wn = deriveOwnership(pod)
	if wk != "ReplicaSet" || wn != "web-abc" {
		t.Fatalf("unexpected: %s/%s", wk, wn)
	}
}
