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
	ArgoCDSchemaVersion       = 1
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
	ClusterUID       *string  `json:"cluster_uid"`
	RunID            string   `json:"run_id"`
	CollectedAt      string   `json:"collected_at"`
	CollectorVersion string   `json:"collector_version"`
	Datasets         Datasets `json:"datasets"`
}

// Datasets holds one self-contained section per dataset.
type Datasets struct {
	Cluster      ClusterSection      `json:"cluster"`
	HelmReleases HelmReleasesSection `json:"helm_releases"`
	PodImages    PodImagesSection    `json:"pod_images"`
	ArgoCD       ArgoCDSection       `json:"argocd"`
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
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
	// NamespacesRequested are the namespaces that existed and matched
	// NAMESPACE_PATTERN at collection time, sorted.
	NamespacesRequested []string `json:"namespaces_requested"`
	// NamespacesDenied is the subset of NamespacesRequested whose pods the
	// agent was refused (403).
	NamespacesDenied []string   `json:"namespaces_denied"`
	Truncated        bool       `json:"truncated"`
	Data             []PodImage `json:"data"`
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

// ArgoCDSection lists the ApplicationSets and Applications that the root
// Argo CD Application (captain-manifests) deploys: the tenant sets it manages
// directly, the Applications those sets generated, and any Application it
// manages directly. Only source identity and status are represented: Helm
// values, valuesObject, parameters, plugin env and condition messages never
// are.
type ArgoCDSection struct {
	SchemaVersion int         `json:"schema_version"`
	Status        string      `json:"status"`
	Error         string      `json:"error,omitempty"`
	Data          *ArgoCDData `json:"data"`
}

// ArgoCDData is the payload of the argocd section.
type ArgoCDData struct {
	// RootApplication is the name of the Application whose managed resources
	// are reported (ARGOCD_ROOT_APPLICATION).
	RootApplication string `json:"root_application"`
	// RootFound is false when that Application (or Argo CD itself) does not
	// exist; both lists are then empty.
	RootFound       bool                 `json:"root_found"`
	ApplicationSets []ArgoApplicationSet `json:"application_sets"`
	Applications    []ArgoApplication    `json:"applications"`
}

// ArgoSource is one entry of an Application's (or a set template's)
// source/sources, reduced to what identifies the chart or manifests.
// RepoURL never carries userinfo.
type ArgoSource struct {
	RepoURL        string  `json:"repo_url"`
	Chart          *string `json:"chart"`
	Path           *string `json:"path"`
	TargetRevision string  `json:"target_revision"`
	Ref            *string `json:"ref"`
}

// ArgoCondition is a status condition without its free-text message.
type ArgoCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// ArgoApplicationSet is one ApplicationSet managed by the root Application.
// Project, destination and sources come from its template, so they may hold
// unrendered template expressions.
type ArgoApplicationSet struct {
	Name      string `json:"application_set_name"`
	Namespace string `json:"namespace"`
	// RootSyncStatus is this set's sync status within the root Application.
	RootSyncStatus *string `json:"root_sync_status"`
	// Generators are the generator kinds (git, list, matrix, ...) only.
	Generators           []string        `json:"generators"`
	Project              string          `json:"project"`
	DestinationServer    *string         `json:"destination_server"`
	DestinationName      *string         `json:"destination_name"`
	DestinationNamespace *string         `json:"destination_namespace"`
	Sources              []ArgoSource    `json:"sources"`
	ApplicationCount     int             `json:"application_count"`
	Conditions           []ArgoCondition `json:"conditions"`
	CreatedAt            *string         `json:"created_at"`
}

// ArgoApplication is one Application generated by a reported set, or managed
// directly by the root Application (ApplicationSet null).
type ArgoApplication struct {
	Name                 string       `json:"application_name"`
	Namespace            string       `json:"namespace"`
	ApplicationSet       *string      `json:"application_set"`
	Project              string       `json:"project"`
	DestinationServer    *string      `json:"destination_server"`
	DestinationName      *string      `json:"destination_name"`
	DestinationNamespace *string      `json:"destination_namespace"`
	Sources              []ArgoSource `json:"sources"`
	SyncStatus           string       `json:"sync_status"`
	// SyncRevisions are the resolved revisions last compared, one per source.
	SyncRevisions  []string `json:"sync_revisions"`
	HealthStatus   string   `json:"health_status"`
	OperationPhase *string  `json:"operation_phase"`
	ReconciledAt   *string  `json:"reconciled_at"`
	CreatedAt      *string  `json:"created_at"`
}
