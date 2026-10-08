package collect

import (
	"context"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/glueops/inventory-agent/internal/schema"
)

// cronJobSuffix matches Job names created by the CronJob controller, which
// appends "-<scheduled time in unix minutes>" (8 digits today; 10 digits
// covers the older unix-seconds scheme and leaves headroom). A Job named
// "<prefix>-<8-10 digits>" is attributed to the CronJob "<prefix>". This is a
// heuristic: a hand-made Job with such a name is misattributed, which is
// acceptable for operational visibility data.
var cronJobSuffix = regexp.MustCompile(`^(.+)-[0-9]{8,10}$`)

// PodImagesParams configures PodImages.
type PodImagesParams struct {
	Namespaces []string
	MaxRows    int
}

// PodImages lists pods in each configured namespace and emits one row per
// container (init and app; ephemeral containers are excluded). A namespace
// the agent may not read is recorded in namespaces_denied; any other list
// error fails the section. Rows beyond MaxRows are cut and truncated is set.
func PodImages(ctx context.Context, client kubernetes.Interface, p PodImagesParams, log *slog.Logger) schema.PodImagesSection {
	section := schema.PodImagesSection{
		SchemaVersion:       schema.PodImagesSchemaVersion,
		NamespacesRequested: append([]string{}, p.Namespaces...),
		NamespacesDenied:    []string{},
	}
	if client == nil {
		return failPods(section, errNoClient, log)
	}

	rows := make([]schema.PodImage, 0, 256)
	truncated := false

namespaces:
	for _, ns := range p.Namespaces {
		list, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			switch {
			case apierrors.IsForbidden(err):
				section.NamespacesDenied = append(section.NamespacesDenied, ns)
				log.Warn("namespace denied", "section", "pod_images", "namespace", ns, "reason", schema.ErrRBACDenied)
				continue
			case apierrors.IsNotFound(err):
				// Should not happen at runtime (the chart only lists
				// namespaces it renders); recorded as denied so the receiver
				// sees the gap, with the real reason in the logs.
				section.NamespacesDenied = append(section.NamespacesDenied, ns)
				log.Warn("namespace not found", "section", "pod_images", "namespace", ns, "reason", "namespace_not_found")
				continue
			default:
				return failPods(section, err, log)
			}
		}
		pods := list.Items
		sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
		for i := range pods {
			for _, row := range podRows(&pods[i]) {
				if p.MaxRows > 0 && len(rows) >= p.MaxRows {
					truncated = true
					break namespaces
				}
				rows = append(rows, row)
			}
		}
	}

	if truncated {
		log.Warn("pod_images truncated", "section", "pod_images", "reason", "row_cap", "max_rows", p.MaxRows)
	}
	section.Status = schema.StatusOK
	section.Truncated = truncated
	section.Data = rows
	return section
}

func failPods(section schema.PodImagesSection, err error, log *slog.Logger) schema.PodImagesSection {
	section.Status = schema.StatusError
	section.Error = Classify(err)
	section.Data = nil
	log.Warn("section failed", "section", "pod_images", "reason", section.Error, "error", err.Error())
	return section
}

// podRows builds one row per init and app container of a pod. Only the
// fields named here are read from the pod; nothing else of the spec is
// touched.
func podRows(pod *corev1.Pod) []schema.PodImage {
	ownerKind, ownerName, workloadKind, workloadName := deriveOwnership(pod)
	nodeName := nullableString(pod.Spec.NodeName)

	initStatus := statusByName(pod.Status.InitContainerStatuses)
	appStatus := statusByName(pod.Status.ContainerStatuses)

	rows := make([]schema.PodImage, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	add := func(c *corev1.Container, ctype string, statuses map[string]*corev1.ContainerStatus) {
		row := schema.PodImage{
			Namespace:     pod.Namespace,
			PodName:       pod.Name,
			PodPhase:      string(pod.Status.Phase),
			NodeName:      nodeName,
			OwnerKind:     ownerKind,
			OwnerName:     ownerName,
			WorkloadKind:  workloadKind,
			WorkloadName:  workloadName,
			ContainerName: c.Name,
			ContainerType: ctype,
			Image:         c.Image,
		}
		if st := statuses[c.Name]; st != nil && st.ImageID != "" {
			row.ImageID = nullableString(st.ImageID)
			row.ImageDigest = Digest(st.ImageID)
		}
		rows = append(rows, row)
	}
	for i := range pod.Spec.InitContainers {
		add(&pod.Spec.InitContainers[i], schema.ContainerTypeInit, initStatus)
	}
	for i := range pod.Spec.Containers {
		add(&pod.Spec.Containers[i], schema.ContainerTypeApp, appStatus)
	}
	return rows
}

// deriveOwnership returns the pod's direct owner (first ownerReference) and
// the workload derived from the pod alone:
//
//	ReplicaSet + pod-template-hash label  -> Deployment (RS name minus "-<hash>")
//	ReplicaSet without the label          -> ReplicaSet
//	Job named "<prefix>-<8-10 digits>"    -> CronJob "<prefix>"
//	DaemonSet / StatefulSet / Job / other -> the owner kind and name as-is
//	no owner                              -> Pod, the pod's own name
func deriveOwnership(pod *corev1.Pod) (ownerKind, ownerName *string, workloadKind, workloadName string) {
	if len(pod.OwnerReferences) == 0 {
		return nil, nil, "Pod", pod.Name
	}
	owner := pod.OwnerReferences[0]
	ownerKind = nullableString(owner.Kind)
	ownerName = nullableString(owner.Name)
	workloadKind, workloadName = owner.Kind, owner.Name

	switch owner.Kind {
	case "ReplicaSet":
		if hash := pod.Labels["pod-template-hash"]; hash != "" && strings.HasSuffix(owner.Name, "-"+hash) {
			workloadKind = "Deployment"
			workloadName = strings.TrimSuffix(owner.Name, "-"+hash)
		}
	case "Job":
		if m := cronJobSuffix.FindStringSubmatch(owner.Name); m != nil {
			workloadKind = "CronJob"
			workloadName = m[1]
		}
	}
	return ownerKind, ownerName, workloadKind, workloadName
}

func statusByName(statuses []corev1.ContainerStatus) map[string]*corev1.ContainerStatus {
	m := make(map[string]*corev1.ContainerStatus, len(statuses))
	for i := range statuses {
		m[statuses[i].Name] = &statuses[i]
	}
	return m
}

// Digest normalises a container runtime imageID to "sha256:<64 hex>".
// Accepted forms: "<repo>@sha256:<hex>", "docker-pullable://<repo>@sha256:<hex>"
// and a bare "sha256:<hex>" (containerd). Anything else yields nil.
func Digest(imageID string) *string {
	s := imageID
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	const prefix = "sha256:"
	if !strings.HasPrefix(s, prefix) || len(s) != len(prefix)+64 || !isLowerHex(s[len(prefix):]) {
		return nil
	}
	return &s
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
