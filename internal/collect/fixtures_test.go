package collect

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/glueops/inventory-agent/internal/logging"
)

// Canary markers planted in fixtures where sensitive data would live
// (values, manifests, env values, non-Helm Secret data). Tests assert none
// of them ever appears in the payload or in the captured logs. They are
// deliberately shaped so secret scanners do not match them.
const (
	markerValues      = "CANARY_HELM_VALUES"
	markerManifest    = "CANARY_HELM_MANIFEST"
	markerHooks       = "CANARY_HELM_HOOKS"
	markerDescription = "CANARY_HELM_DESCRIPTION"
	markerPodEnv      = "CANARY_POD_ENV_VALUE"
	markerPodArgs     = "CANARY_POD_ARGS_VALUE"
	markerOtherSecret = "CANARY_ARGOCD_ADMIN_VALUE"
)

var sensitiveMarkers = []string{
	markerValues, markerManifest, markerHooks, markerDescription,
	markerPodEnv, markerPodArgs, markerOtherSecret,
}

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return logging.New("debug", &buf), &buf
}

// helmReleaseJSON returns a realistic Helm release document including all
// the fields the agent must discard.
func helmReleaseJSON(name, ns string, revision int, status, chart, chartVer, appVer, first, last string) map[string]any {
	return map[string]any{
		"name":      name,
		"namespace": ns,
		"version":   revision,
		"info": map[string]any{
			"first_deployed": first,
			"last_deployed":  last,
			"deleted":        "",
			"description":    markerDescription,
			"status":         status,
		},
		"chart": map[string]any{
			"metadata": map[string]any{
				"name":       chart,
				"version":    chartVer,
				"appVersion": appVer,
				"apiVersion": "v2",
			},
			"templates": []map[string]any{{"name": "templates/deployment.yaml", "data": "dGVtcGxhdGU="}},
			"values":    map[string]any{"password": markerValues},
		},
		"config":   map[string]any{"adminPassword": markerValues},
		"manifest": "---\n# Source: x\n" + markerManifest + "\n",
		"hooks":    []map[string]any{{"name": "pre-install", "manifest": markerHooks}},
	}
}

func encodeRelease(t *testing.T, rel map[string]any, gz bool) []byte {
	t.Helper()
	raw, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	payload := raw
	if gz {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		payload = buf.Bytes()
	}
	// Helm base64-encodes before handing the bytes to the Secret; this is the
	// text that ends up in Secret.Data after client-go's own decoding.
	return []byte(base64.StdEncoding.EncodeToString(payload))
}

func helmSecret(t *testing.T, ns, name string, revision int, rel map[string]any) *corev1.Secret {
	t.Helper()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, revision),
			Namespace: ns,
			Labels: map[string]string{
				"owner":   "helm",
				"name":    name,
				"version": strconv.Itoa(revision),
				"status":  fmt.Sprint(rel["info"].(map[string]any)["status"]),
			},
		},
		Type: helmSecretType,
		Data: map[string][]byte{"release": encodeRelease(t, rel, true)},
	}
}

func otherSecret(ns string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "argocd-secret", Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"admin.password": []byte(markerOtherSecret)},
	}
}

// podFixture is a compact way to describe test pods.
type podFixture struct {
	ns, name, phase, node string
	owner                 *metav1.OwnerReference
	labels                map[string]string
	inits, apps           []string          // "name=image"
	imageIDs              map[string]string // container name -> imageID
}

func ownerRef(kind, name string) *metav1.OwnerReference {
	ctrl := true
	return &metav1.OwnerReference{APIVersion: "apps/v1", Kind: kind, Name: name, Controller: &ctrl}
}

func (f podFixture) build() *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: f.name, Namespace: f.ns, Labels: f.labels},
		Spec:       corev1.PodSpec{NodeName: f.node},
		Status:     corev1.PodStatus{Phase: corev1.PodPhase(f.phase)},
	}
	if f.owner != nil {
		pod.OwnerReferences = []metav1.OwnerReference{*f.owner}
	}
	mk := func(spec string) corev1.Container {
		name, image, _ := bytes.Cut([]byte(spec), []byte("="))
		return corev1.Container{
			Name:  string(name),
			Image: string(image),
			// Fields the agent must never read or emit.
			Env:     []corev1.EnvVar{{Name: "AWS_SECRET_ACCESS_KEY", Value: markerPodEnv}},
			Args:    []string{markerPodArgs},
			Command: []string{"/bin/run"},
		}
	}
	for _, s := range f.inits {
		c := mk(s)
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, c)
		if id, ok := f.imageIDs[c.Name]; ok {
			pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses,
				corev1.ContainerStatus{Name: c.Name, Image: c.Image, ImageID: id})
		}
	}
	for _, s := range f.apps {
		c := mk(s)
		pod.Spec.Containers = append(pod.Spec.Containers, c)
		if id, ok := f.imageIDs[c.Name]; ok {
			pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses,
				corev1.ContainerStatus{Name: c.Name, Image: c.Image, ImageID: id})
		}
	}
	// Ephemeral containers must be ignored entirely.
	pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debugger", Image: "busybox:ephemeral"},
	}}
	return pod
}

const (
	digestCoreDNS = "sha256:900f9c109f7a33545d3c811516e8376df9019147b750f5ce3e254468769176ea"
	digestCSI     = "sha256:7ca92c0a7b4f1c51d306409da6b832005fcadb224e384795646376726734c6e5"
	digestNode    = "sha256:e757967a5ec338f6a9b371c5a9688bedaa8c3578ea3dd4db329ea0084be0a86f"
)

// k3dAndEKSPods is the shared pod fixture set covering every ownership and
// status case the spec names.
func k3dAndEKSPods() []*corev1.Pod {
	fixtures := []podFixture{
		{ // Deployment via ReplicaSet + pod-template-hash (k3d)
			ns: "kube-system", name: "coredns-c5fdd76cf-bb6t2", phase: "Running", node: "k3d-captain-agent-4",
			owner: ownerRef("ReplicaSet", "coredns-c5fdd76cf"), labels: map[string]string{"pod-template-hash": "c5fdd76cf"},
			apps:     []string{"coredns=rancher/mirrored-coredns-coredns:1.14.6"},
			imageIDs: map[string]string{"coredns": "docker.io/rancher/mirrored-coredns-coredns@" + digestCoreDNS},
		},
		{ // DaemonSet (EKS aws-node) with a bare containerd sha256 imageID (no repo digest)
			ns: "kube-system", name: "aws-node-x7k2p", phase: "Running", node: "ip-10-0-1-23.ec2.internal",
			owner: ownerRef("DaemonSet", "aws-node"),
			apps:  []string{"aws-node=602401143452.dkr.ecr.us-east-1.amazonaws.com/amazon-k8s-cni:v1.19.0"},
			imageIDs: map[string]string{
				"aws-node": digestNode,
			},
		},
		{ // DaemonSet (EKS kube-proxy)
			ns: "kube-system", name: "kube-proxy-9hq4d", phase: "Running", node: "ip-10-0-1-23.ec2.internal",
			owner:    ownerRef("DaemonSet", "kube-proxy"),
			apps:     []string{"kube-proxy=602401143452.dkr.ecr.us-east-1.amazonaws.com/eks/kube-proxy:v1.31.0"},
			imageIDs: map[string]string{"kube-proxy": "602401143452.dkr.ecr.us-east-1.amazonaws.com/eks/kube-proxy@" + digestCSI},
		},
		{ // EKS ebs-csi-node DaemonSet with init containers
			ns: "kube-system", name: "ebs-csi-node-4jk9z", phase: "Running", node: "ip-10-0-1-23.ec2.internal",
			owner: ownerRef("DaemonSet", "ebs-csi-node"),
			inits: []string{"init-dir=public.ecr.aws/eks-distro/kubernetes-csi/livenessprobe:v2.14.0"},
			apps: []string{
				"ebs-plugin=public.ecr.aws/ebs-csi-driver/aws-ebs-csi-driver:v1.37.0",
				"node-driver-registrar=public.ecr.aws/eks-distro/kubernetes-csi/node-driver-registrar:v2.13.0",
			},
			imageIDs: map[string]string{
				"init-dir":   "public.ecr.aws/eks-distro/kubernetes-csi/livenessprobe@" + digestCSI,
				"ebs-plugin": "public.ecr.aws/ebs-csi-driver/aws-ebs-csi-driver@" + digestCSI,
				// node-driver-registrar: legacy dockershim format, digest still extractable
				"node-driver-registrar": "docker-pullable://public.ecr.aws/eks-distro/kubernetes-csi/node-driver-registrar@" + digestNode,
			},
		},
		{ // StatefulSet
			ns: "glueops-core", name: "loki-0", phase: "Running", node: "k3d-captain-agent-1",
			owner:    ownerRef("StatefulSet", "loki"),
			apps:     []string{"loki=grafana/loki:3.4.1"},
			imageIDs: map[string]string{"loki": "docker.io/grafana/loki@" + digestCSI},
		},
		{ // Plain Job (no timestamp suffix)
			ns: "glueops-core", name: "one-off-migrate-abcde", phase: "Succeeded", node: "k3d-captain-agent-2",
			owner:    ownerRef("Job", "one-off-migrate"),
			apps:     []string{"migrate=ghcr.io/glueops/migrate:1.0.0"},
			imageIDs: map[string]string{"migrate": "ghcr.io/glueops/migrate@" + digestCSI},
		},
		{ // Job created by a CronJob (8-digit unix-minutes suffix)
			ns: "glueops-core", name: "backups-and-exports-29837460-k2x9p", phase: "Succeeded", node: "k3d-captain-agent-2",
			owner:    ownerRef("Job", "backups-and-exports-29837460"),
			apps:     []string{"backup=ghcr.io/glueops/backups:2.3.4"},
			imageIDs: map[string]string{"backup": "ghcr.io/glueops/backups@" + digestCSI},
		},
		{ // Ownerless pod
			ns: "glueops-core", name: "debug-shell", phase: "Running", node: "k3d-captain-agent-3",
			apps:     []string{"shell=busybox:1.36"},
			imageIDs: map[string]string{"shell": "docker.io/library/busybox@" + digestNode},
		},
		{ // Pending pod: no node, no statuses
			ns: "glueops-core", name: "pending-7d9f8-zzzzz", phase: "Pending",
			owner: ownerRef("ReplicaSet", "pending-7d9f8"), labels: map[string]string{"pod-template-hash": "7d9f8"},
			inits: []string{"wait=busybox:1.36"},
			apps:  []string{"app=ghcr.io/glueops/app:0.1.0"},
		},
		{ // ReplicaSet without pod-template-hash stays ReplicaSet
			ns: "glueops-core", name: "bare-rs-abcde", phase: "Running", node: "k3d-captain-agent-3",
			owner:    ownerRef("ReplicaSet", "bare-rs"),
			apps:     []string{"app=ghcr.io/glueops/app:0.1.0"},
			imageIDs: map[string]string{"app": "docker://legacy-opaque-id-without-digest"},
		},
	}
	pods := make([]*corev1.Pod, 0, len(fixtures))
	for _, f := range fixtures {
		pods = append(pods, f.build())
	}
	return pods
}

func assertNoMarkers(t *testing.T, label string, text string) {
	t.Helper()
	for _, m := range sensitiveMarkers {
		if bytes.Contains([]byte(text), []byte(m)) {
			t.Errorf("%s contains sensitive marker %q", label, m)
		}
	}
}
