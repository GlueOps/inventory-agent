package collect

import (
	"context"
	"log/slog"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/glueops/inventory-agent/internal/schema"
)

// Cluster collects GET /version and node.status.nodeInfo for every node the
// API lists. Nothing else from the Node object is read.
func Cluster(ctx context.Context, client kubernetes.Interface, log *slog.Logger) schema.ClusterSection {
	section := schema.ClusterSection{SchemaVersion: schema.ClusterSchemaVersion}
	data, err := collectCluster(ctx, client)
	if err != nil {
		section.Status = schema.StatusError
		section.Error = Classify(err)
		log.Warn("section failed", "section", "cluster", "reason", section.Error, "error", err.Error())
		return section
	}
	section.Status = schema.StatusOK
	section.Data = data
	return section
}

func collectCluster(ctx context.Context, client kubernetes.Interface) (*schema.ClusterData, error) {
	if client == nil {
		return nil, errNoClient
	}
	ver, err := client.Discovery().ServerVersion()
	if err != nil {
		return nil, err
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	data := &schema.ClusterData{
		K8sVersion: ver.GitVersion,
		K8sMajor:   ver.Major,
		K8sMinor:   ver.Minor,
		Platform:   ver.Platform,
		Nodes:      make([]schema.Node, 0, len(nodes.Items)),
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		info := n.Status.NodeInfo
		data.Nodes = append(data.Nodes, schema.Node{
			NodeName:                n.Name,
			KubeletVersion:          info.KubeletVersion,
			OSImage:                 info.OSImage,
			OperatingSystem:         info.OperatingSystem,
			Architecture:            info.Architecture,
			KernelVersion:           info.KernelVersion,
			ContainerRuntimeVersion: info.ContainerRuntimeVersion,
		})
	}
	sort.Slice(data.Nodes, func(i, j int) bool { return data.Nodes[i].NodeName < data.Nodes[j].NodeName })
	return data, nil
}

// ClusterUID returns the UID of the kube-system Namespace (a single GET, so
// the RBAC grant can be limited with resourceNames).
func ClusterUID(ctx context.Context, client kubernetes.Interface) (string, error) {
	if client == nil {
		return "", errNoClient
	}
	ns, err := client.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return string(ns.UID), nil
}
