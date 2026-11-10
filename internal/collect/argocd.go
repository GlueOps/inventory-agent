package collect

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/glueops/inventory-agent/internal/schema"
)

const argoGroup = "argoproj.io"

var (
	// ApplicationGVR and ApplicationSetGVR are Argo CD's resources; exported
	// for tests that build a fake dynamic client.
	ApplicationGVR    = k8sschema.GroupVersionResource{Group: argoGroup, Version: "v1alpha1", Resource: "applications"}
	ApplicationSetGVR = k8sschema.GroupVersionResource{Group: argoGroup, Version: "v1alpha1", Resource: "applicationsets"}
)

// ArgoCDParams select what the argocd section reports.
type ArgoCDParams struct {
	// Namespace is where Argo CD keeps its Applications and ApplicationSets.
	Namespace string
	// RootApplication is the Application whose managed sets and apps are
	// reported (captain-manifests).
	RootApplication string
}

// The closed views below are everything the agent decodes from an
// Application or ApplicationSet. encoding/json drops every field not declared
// here, so Helm values, valuesObject, parameters, plugin env, generator
// contents and condition messages are never materialised as Go values.

type argoSourceView struct {
	RepoURL        string `json:"repoURL"`
	Chart          string `json:"chart"`
	Path           string `json:"path"`
	TargetRevision string `json:"targetRevision"`
	Ref            string `json:"ref"`
}

type argoMetaView struct {
	Name              string `json:"name"`
	Namespace         string `json:"namespace"`
	CreationTimestamp string `json:"creationTimestamp"`
	OwnerReferences   []struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		Controller *bool  `json:"controller"`
	} `json:"ownerReferences"`
}

type argoAppSpecView struct {
	Project     string `json:"project"`
	Destination struct {
		Server    string `json:"server"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"destination"`
	Source  *argoSourceView  `json:"source"`
	Sources []argoSourceView `json:"sources"`
}

type argoAppView struct {
	Metadata argoMetaView    `json:"metadata"`
	Spec     argoAppSpecView `json:"spec"`
	Status   struct {
		Sync struct {
			Status    string   `json:"status"`
			Revision  string   `json:"revision"`
			Revisions []string `json:"revisions"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		OperationState *struct {
			Phase string `json:"phase"`
		} `json:"operationState"`
		ReconciledAt string `json:"reconciledAt"`
		// Resources is read only on the root Application.
		Resources []struct {
			Group     string `json:"group"`
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
			Status    string `json:"status"`
		} `json:"resources"`
	} `json:"status"`
}

type argoAppSetView struct {
	Metadata argoMetaView `json:"metadata"`
	Spec     struct {
		// Only the keys are used: each generator is {"<kind>": {...}}, and
		// struct{} discards the contents (list elements can hold anything).
		Generators []map[string]struct{} `json:"generators"`
		Template   struct {
			Spec argoAppSpecView `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"conditions"`
	} `json:"status"`
}

// ArgoCD reports the ApplicationSets and Applications the root Application
// manages. A missing root Application (or no Argo CD at all) is ok with
// root_found=false; an Application or set that cannot be decoded is skipped
// with a warning and counted. skipped is logged by the caller.
func ArgoCD(ctx context.Context, client dynamic.Interface, p ArgoCDParams, log *slog.Logger) (section schema.ArgoCDSection, skipped int) {
	section = schema.ArgoCDSection{SchemaVersion: schema.ArgoCDSchemaVersion}
	data, skipped, err := collectArgoCD(ctx, client, p, log)
	if err != nil {
		section.Status = schema.StatusError
		section.Error = Classify(err)
		log.Warn("section failed", "section", "argocd", "reason", section.Error, "error", err.Error())
		return section, skipped
	}
	section.Status = schema.StatusOK
	section.Data = data
	return section, skipped
}

func collectArgoCD(ctx context.Context, client dynamic.Interface, p ArgoCDParams, log *slog.Logger) (*schema.ArgoCDData, int, error) {
	if client == nil {
		return nil, 0, errNoClient
	}
	data := &schema.ArgoCDData{
		RootApplication: p.RootApplication,
		ApplicationSets: []schema.ArgoApplicationSet{},
		Applications:    []schema.ArgoApplication{},
	}

	rootObj, err := client.Resource(ApplicationGVR).Namespace(p.Namespace).Get(ctx, p.RootApplication, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// No root Application, or no Argo CD CRDs: nothing is deployed
		// through it, which is a true empty, not a failure.
		return data, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var root argoAppView
	if err := decodeArgo(rootObj, &root); err != nil {
		return nil, 0, err
	}
	data.RootFound = true

	// What the root manages, from its status.resources. Only Argo CD
	// objects in the Argo CD namespace are followed: the agent can read
	// nothing else.
	wantSets := map[string]*string{}
	wantApps := map[string]bool{}
	for _, r := range root.Status.Resources {
		ns := r.Namespace
		if ns == "" {
			ns = p.Namespace
		}
		if r.Group != argoGroup || ns != p.Namespace {
			continue
		}
		status := r.Status
		switch r.Kind {
		case "ApplicationSet":
			wantSets[r.Name] = optional(status)
		case "Application":
			if r.Name != p.RootApplication {
				wantApps[r.Name] = true
			}
		}
	}
	if len(wantSets) == 0 && len(wantApps) == 0 {
		return data, 0, nil
	}

	var skipped int
	skip := func(kind, name string, err error) {
		skipped++
		log.Warn("argo cd object skipped", "section", "argocd", "reason", schema.ErrDecodeFailed,
			"kind", kind, "name", name, "error", err.Error())
	}

	sets := map[string]schema.ArgoApplicationSet{}
	if len(wantSets) > 0 {
		list, err := client.Resource(ApplicationSetGVR).Namespace(p.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, 0, err
		}
		if list != nil {
			for i := range list.Items {
				obj := &list.Items[i]
				syncStatus, ok := wantSets[obj.GetName()]
				if !ok {
					continue
				}
				var v argoAppSetView
				if err := decodeArgo(obj, &v); err != nil {
					skip("ApplicationSet", obj.GetName(), err)
					continue
				}
				sets[v.Metadata.Name] = applicationSetRow(&v, syncStatus, p.Namespace)
			}
		}
	}

	list, err := client.Resource(ApplicationGVR).Namespace(p.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, 0, err
	}
	for i := range list.Items {
		obj := &list.Items[i]
		owner := controllingSet(obj)
		_, ownedByReported := sets[owner]
		if !ownedByReported && !wantApps[obj.GetName()] {
			continue
		}
		var v argoAppView
		if err := decodeArgo(obj, &v); err != nil {
			skip("Application", obj.GetName(), err)
			continue
		}
		row := applicationRow(&v, p.Namespace)
		if ownedByReported {
			row.ApplicationSet = optional(owner)
			s := sets[owner]
			s.ApplicationCount++
			sets[owner] = s
		}
		data.Applications = append(data.Applications, row)
	}

	for _, s := range sets {
		data.ApplicationSets = append(data.ApplicationSets, s)
	}
	sort.Slice(data.ApplicationSets, func(i, j int) bool { return data.ApplicationSets[i].Name < data.ApplicationSets[j].Name })
	sort.Slice(data.Applications, func(i, j int) bool { return data.Applications[i].Name < data.Applications[j].Name })
	return data, skipped, nil
}

// controllingSet returns the name of the ApplicationSet that controls obj, or
// "" when none does.
func controllingSet(obj *unstructured.Unstructured) string {
	for _, o := range obj.GetOwnerReferences() {
		if o.Kind == "ApplicationSet" && strings.HasPrefix(o.APIVersion, argoGroup+"/") && o.Controller != nil && *o.Controller {
			return o.Name
		}
	}
	return ""
}

// decodeArgo round-trips an object through JSON into a closed view. The error
// names only the stage, never content.
func decodeArgo(obj *unstructured.Unstructured, into any) error {
	raw, err := json.Marshal(obj.Object)
	if err != nil {
		return &DecodeError{Kind: obj.GetKind(), Secret: obj.GetName(), Stage: "marshal", Err: err}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return &DecodeError{Kind: obj.GetKind(), Secret: obj.GetName(), Stage: "json", Err: err}
	}
	return nil
}

func applicationSetRow(v *argoAppSetView, rootSyncStatus *string, defaultNS string) schema.ArgoApplicationSet {
	gens := []string{}
	for _, g := range v.Spec.Generators {
		keys := make([]string, 0, len(g))
		for k := range g {
			if k == "selector" { // a generator-level filter, not a kind
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		gens = append(gens, keys...)
	}
	conds := []schema.ArgoCondition{}
	for _, c := range v.Status.Conditions {
		conds = append(conds, schema.ArgoCondition{Type: c.Type, Status: c.Status, Reason: c.Reason})
	}
	sort.Slice(conds, func(i, j int) bool { return conds[i].Type < conds[j].Type })
	spec := &v.Spec.Template.Spec
	return schema.ArgoApplicationSet{
		Name:                 v.Metadata.Name,
		Namespace:            orDefault(v.Metadata.Namespace, defaultNS),
		RootSyncStatus:       rootSyncStatus,
		Generators:           gens,
		Project:              spec.Project,
		DestinationServer:    optional(spec.Destination.Server),
		DestinationName:      optional(spec.Destination.Name),
		DestinationNamespace: optional(spec.Destination.Namespace),
		Sources:              sources(spec),
		Conditions:           conds,
		CreatedAt:            parseKubeTime(v.Metadata.CreationTimestamp),
	}
}

func applicationRow(v *argoAppView, defaultNS string) schema.ArgoApplication {
	revs := v.Status.Sync.Revisions
	if len(revs) == 0 && v.Status.Sync.Revision != "" {
		revs = []string{v.Status.Sync.Revision}
	}
	if revs == nil {
		revs = []string{}
	}
	var phase *string
	if v.Status.OperationState != nil {
		phase = optional(v.Status.OperationState.Phase)
	}
	return schema.ArgoApplication{
		Name:                 v.Metadata.Name,
		Namespace:            orDefault(v.Metadata.Namespace, defaultNS),
		Project:              v.Spec.Project,
		DestinationServer:    optional(v.Spec.Destination.Server),
		DestinationName:      optional(v.Spec.Destination.Name),
		DestinationNamespace: optional(v.Spec.Destination.Namespace),
		Sources:              sources(&v.Spec),
		SyncStatus:           v.Status.Sync.Status,
		SyncRevisions:        revs,
		HealthStatus:         v.Status.Health.Status,
		OperationPhase:       phase,
		ReconciledAt:         parseKubeTime(v.Status.ReconciledAt),
		CreatedAt:            parseKubeTime(v.Metadata.CreationTimestamp),
	}
}

// sources returns spec.sources, or spec.source as a one-element list.
func sources(spec *argoAppSpecView) []schema.ArgoSource {
	in := spec.Sources
	if len(in) == 0 && spec.Source != nil {
		in = []argoSourceView{*spec.Source}
	}
	out := make([]schema.ArgoSource, 0, len(in))
	for _, s := range in {
		out = append(out, schema.ArgoSource{
			RepoURL:        StripUserinfo(s.RepoURL),
			Chart:          optional(s.Chart),
			Path:           optional(s.Path),
			TargetRevision: s.TargetRevision,
			Ref:            optional(s.Ref),
		})
	}
	return out
}

// StripUserinfo removes credentials from a repository URL: the userinfo of
// a URL (https://user:token@host/...) and a user:password prefix of an
// scp-style address (user:token@host:path). A plain scp user (git@host:path)
// is kept: it is not a secret.
func StripUserinfo(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		if u.User == nil {
			return raw
		}
		u.User = nil
		return u.String()
	}
	if at := strings.LastIndex(raw, "@"); at >= 0 && strings.Contains(raw[:at], ":") {
		return raw[at+1:]
	}
	return raw
}

func parseKubeTime(s string) *string {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return formatTimePtr(t)
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
