// Package schema defines the Go types that ARE the inventory payload.
//
// Every struct here is closed: there are no map or interface fields, so a
// field can only reach the wire if it is declared below with a json tag. The
// JSON Schema in schema/payload.schema.json mirrors these types with
// additionalProperties: false at every level, and the tests in this package
// keep the two in sync and validate a full sample envelope against it.
//
// Versioning rules (from the spec): adding an optional field is not a bump;
// renaming, removing or changing the type of a field bumps the affected
// schema_version.
package schema

// Schema versions, one per envelope and one per dataset section.
const (
	EnvelopeSchemaVersion     = 1
	ClusterSchemaVersion      = 1
	HelmReleasesSchemaVersion = 1
	PodImagesSchemaVersion    = 1
)

// Section statuses.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// Section error codes. These are the only values the `error` field may hold.
const (
	ErrRBACDenied     = "rbac_denied"
	ErrAPIUnavailable = "api_unavailable"
	ErrDecodeFailed   = "decode_failed"
	ErrInternal       = "internal_error"
)

// Container types for pod_images rows.
const (
	ContainerTypeApp  = "app"
	ContainerTypeInit = "init"
)

// Envelope is the single JSON document POSTed once per run.
type Envelope struct {
	SchemaVersion int    `json:"schema_version"`
	CaptainDomain string `json:"captain_domain"`
	// ClusterUID is the UID of the kube-system Namespace. It is null only when
	// the lookup failed; the failure is logged with a reason code.
	ClusterUID           *string  `json:"cluster_uid"`
	RunID                string   `json:"run_id"`
	CollectedAt          string   `json:"collected_at"`
	CollectorVersion     string   `json:"collector_version"`
	PlatformChartVersion string   `json:"platform_chart_version"`
	Datasets             Datasets `json:"datasets"`
}

// Datasets holds one self-contained section per dataset.
type Datasets struct {
	Cluster      ClusterSection      `json:"cluster"`
	HelmReleases HelmReleasesSection `json:"helm_releases"`
	PodImages    PodImagesSection    `json:"pod_images"`
}

// ClusterSection carries distribution-agnostic cluster and node version info.
type ClusterSection struct {
	SchemaVersion int          `json:"schema_version"`
	Status        string       `json:"status"`
	Error         string       `json:"error,omitempty"`
	Data          *ClusterData `json:"data"`
}

// ClusterData is the payload of the cluster section.
type ClusterData struct {
	K8sVersion string `json:"k8s_version"`
	K8sMajor   string `json:"k8s_major"`
	K8sMinor   string `json:"k8s_minor"`
	Platform   string `json:"platform"`
	Nodes      []Node `json:"nodes"`
}

// Node is one entry per node the API lists, taken from node.status.nodeInfo.
type Node struct {
	NodeName                string `json:"node_name"`
	KubeletVersion          string `json:"kubelet_version"`
	OSImage                 string `json:"os_image"`
	OperatingSystem         string `json:"operating_system"`
	Architecture            string `json:"architecture"`
	KernelVersion           string `json:"kernel_version"`
	ContainerRuntimeVersion string `json:"container_runtime_version"`
}

// HelmReleasesSection lists the Helm-installed releases in the Helm namespace.
type HelmReleasesSection struct {
	SchemaVersion int           `json:"schema_version"`
	Status        string        `json:"status"`
	Error         string        `json:"error,omitempty"`
	Data          []HelmRelease `json:"data"`
}

// HelmRelease is the nine-field metadata extract of one Helm release.
// Values, manifests, hooks and the description are never represented here.
type HelmRelease struct {
	ReleaseName   string  `json:"release_name"`
	Namespace     string  `json:"namespace"`
	ChartName     string  `json:"chart_name"`
	ChartVersion  string  `json:"chart_version"`
	AppVersion    string  `json:"app_version"`
	Revision      int     `json:"revision"`
	Status        string  `json:"status"`
	FirstDeployed *string `json:"first_deployed"`
	LastDeployed  *string `json:"last_deployed"`
}

// PodImagesSection lists one row per container per pod in the platform
// namespaces.
type PodImagesSection struct {
	SchemaVersion       int        `json:"schema_version"`
	Status              string     `json:"status"`
	Error               string     `json:"error,omitempty"`
	NamespacesRequested []string   `json:"namespaces_requested"`
	NamespacesDenied    []string   `json:"namespaces_denied"`
	Truncated           bool       `json:"truncated"`
	Data                []PodImage `json:"data"`
}

// PodImage is one container of one pod. Nothing from the pod spec beyond
// image identity is represented here.
type PodImage struct {
	Namespace     string  `json:"namespace"`
	PodName       string  `json:"pod_name"`
	PodPhase      string  `json:"pod_phase"`
	NodeName      *string `json:"node_name"`
	OwnerKind     *string `json:"owner_kind"`
	OwnerName     *string `json:"owner_name"`
	WorkloadKind  string  `json:"workload_kind"`
	WorkloadName  string  `json:"workload_name"`
	ContainerName string  `json:"container_name"`
	ContainerType string  `json:"container_type"`
	Image         string  `json:"image"`
	ImageID       *string `json:"image_id"`
	ImageDigest   *string `json:"image_digest"`
}
