package collect

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	payload "github.com/glueops/inventory-agent/internal/schema"
)

var argoParams = ArgoCDParams{Namespace: "glueops-core", RootApplication: "captain-manifests"}

// argoClient is a fake dynamic client that knows Argo CD's list kinds.
func argoClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[k8sschema.GroupVersionResource]string{
		ApplicationGVR:    "ApplicationList",
		ApplicationSetGVR: "ApplicationSetList",
	}, objs...)
}

func argoObj(kind, ns, name string, fields map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       kind,
		"metadata": map[string]any{
			"name":              name,
			"namespace":         ns,
			"creationTimestamp": "2026-11-04T05:58:13Z",
		},
	}
	for k, v := range fields {
		obj[k] = v
	}
	return &unstructured.Unstructured{Object: obj}
}

func ownedBy(u *unstructured.Unstructured, set string, controller bool) *unstructured.Unstructured {
	meta := u.Object["metadata"].(map[string]any)
	meta["ownerReferences"] = []any{map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationSet", "name": set,
		"uid": "930fbf51-8930-48ad-8e8c-61ba5a712ac0", "controller": controller,
	}}
	return u
}

func rootApp(resources ...map[string]any) *unstructured.Unstructured {
	rs := make([]any, 0, len(resources))
	for _, r := range resources {
		rs = append(rs, r)
	}
	return argoObj("Application", "glueops-core", "captain-manifests", map[string]any{
		"spec": map[string]any{
			"project": "default",
			"source":  map[string]any{"repoURL": "git@github.com:development-captains/nonprod.venus.onglueops.rocks.git", "path": "manifests/", "targetRevision": "HEAD"},
		},
		"status": map[string]any{"resources": rs},
	})
}

func managed(group, kind, ns, name, status string) map[string]any {
	return map[string]any{"group": group, "version": "v1alpha1", "kind": kind, "namespace": ns, "name": name, "status": status}
}

// appSources is the two-source tenant layout: the app chart with values
// files and inline values, plus the $values ref repo.
func appSources(rev string) []any {
	return []any{
		map[string]any{
			"repoURL":        "https://github.com/GlueOps/project-template-helm-chart-app.git",
			"path":           ".",
			"targetRevision": rev,
			"helm": map[string]any{
				"valueFiles":   []any{"$values/common/common-values.yaml"},
				"values":       "api_key: " + markerArgoValues,
				"valuesObject": map[string]any{"token": markerArgoValuesObj},
				"parameters":   []any{map[string]any{"name": "secret", "value": markerArgoParameter}},
			},
		},
		map[string]any{
			"repoURL":        "https://deploy:" + markerArgoRepoSecret + "@github.com/development-tenant-venus/deployment-configurations",
			"targetRevision": "main",
			"ref":            "values",
		},
	}
}

func tenantApp(name, set string) *unstructured.Unstructured {
	u := argoObj("Application", "glueops-core", name, map[string]any{
		"spec": map[string]any{
			"project":     "nonprod",
			"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "nonprod"},
			"sources":     appSources("v0.12.0-rc1"),
		},
		"status": map[string]any{
			"sync":           map[string]any{"status": "Synced", "revisions": []any{"de2287f9", "9e600687"}},
			"health":         map[string]any{"status": "Healthy"},
			"operationState": map[string]any{"phase": "Succeeded", "message": markerArgoCondition},
			"reconciledAt":   "2026-11-10T06:11:21Z",
			"conditions":     []any{map[string]any{"type": "SyncError", "message": markerArgoCondition}},
		},
	})
	if set != "" {
		ownedBy(u, set, true)
	}
	return u
}

func tenantSet(name string) *unstructured.Unstructured {
	return argoObj("ApplicationSet", "glueops-core", name, map[string]any{
		"spec": map[string]any{
			"goTemplate": true,
			"generators": []any{
				map[string]any{"git": map[string]any{"repoURL": "https://github.com/development-tenant-venus/deployment-configurations", "revision": "HEAD"}},
				map[string]any{"list": map[string]any{"elements": []any{map[string]any{"secret": markerArgoGenerator}}}},
			},
			"template": map[string]any{
				"metadata": map[string]any{"name": "{{ .path.basename }}"},
				"spec": map[string]any{
					"project":     "nonprod",
					"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "nonprod"},
					"sources":     appSources("v0.12.0-rc1"),
				},
			},
		},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "ResourcesUpToDate", "status": "True", "reason": "ApplicationSetUpToDate", "message": markerArgoCondition},
			map[string]any{"type": "ErrorOccurred", "status": "False", "reason": "ApplicationSetUpToDate", "message": markerArgoCondition},
		}},
	})
}

// venusLike mirrors the venus captain: captain-manifests manages one tenant
// set (plus a ConfigMap, Namespace and AppProject) and one Application
// directly; a stray set and its app, an app owned by a non-controller
// reference and a set in another namespace must all be left out.
func venusLike() []runtime.Object {
	direct := argoObj("Application", "glueops-core", "direct-app", map[string]any{
		"spec": map[string]any{
			"project":     "default",
			"destination": map[string]any{"name": "in-cluster", "namespace": "tools"},
			"source": map[string]any{
				"repoURL": "https://helm.gpkg.io/project-template", "chart": "app", "targetRevision": "0.17.1",
				"plugin": map[string]any{"env": []any{map[string]any{"name": "X", "value": markerArgoPluginEnv}}},
			},
		},
		"status": map[string]any{
			"sync":   map[string]any{"status": "OutOfSync", "revision": "0.17.1"},
			"health": map[string]any{"status": "Missing"},
		},
	})
	return []runtime.Object{
		rootApp(
			managed("", "ConfigMap", "glueops-core-monitoring", "dash", "OutOfSync"),
			managed("", "Namespace", "", "nonprod", "Synced"),
			managed("argoproj.io", "AppProject", "glueops-core", "nonprod", "Synced"),
			managed("argoproj.io", "ApplicationSet", "glueops-core", "nonprod-application-set", "Synced"),
			managed("argoproj.io", "ApplicationSet", "elsewhere", "other-ns-set", "Synced"),
			managed("argoproj.io", "Application", "glueops-core", "direct-app", "Synced"),
			managed("argoproj.io", "Application", "glueops-core", "captain-manifests", "Synced"),
		),
		tenantSet("nonprod-application-set"),
		tenantApp("front-end-prod", "nonprod-application-set"),
		tenantApp("back-end-prod", "nonprod-application-set"),
		direct,
		tenantSet("stray-set"),
		tenantApp("stray-app", "stray-set"),
		ownedBy(tenantApp("adopted-app", ""), "nonprod-application-set", false),
		argoObj("ApplicationSet", "elsewhere", "other-ns-set", nil),
		argoObj("Application", "glueops-core", "glueops-core-cert-manager", map[string]any{"spec": map[string]any{"project": "glueops-core"}}),
	}
}

func str(s string) *string { return &s }

func TestArgoCDReportsWhatTheRootManages(t *testing.T) {
	log, logs := testLogger()
	section, skipped := ArgoCD(context.Background(), argoClient(venusLike()...), argoParams, log)
	if section.Status != payload.StatusOK || skipped != 0 || section.Data == nil {
		t.Fatalf("unexpected: %+v skipped=%d", section, skipped)
	}
	d := section.Data
	if d.RootApplication != "captain-manifests" || !d.RootFound {
		t.Fatalf("root: %+v", d)
	}

	if len(d.ApplicationSets) != 1 {
		t.Fatalf("want only the managed set, got %+v", d.ApplicationSets)
	}
	set := d.ApplicationSets[0]
	wantSources := []payload.ArgoSource{
		{RepoURL: "https://github.com/GlueOps/project-template-helm-chart-app.git", Path: str("."), TargetRevision: "v0.12.0-rc1"},
		{RepoURL: "https://github.com/development-tenant-venus/deployment-configurations", TargetRevision: "main", Ref: str("values")},
	}
	want := payload.ArgoApplicationSet{
		Name: "nonprod-application-set", Namespace: "glueops-core", RootSyncStatus: str("Synced"),
		Generators: []string{"git", "list"}, Project: "nonprod",
		DestinationServer: str("https://kubernetes.default.svc"), DestinationNamespace: str("nonprod"),
		Sources: wantSources, ApplicationCount: 2,
		Conditions: []payload.ArgoCondition{
			{Type: "ErrorOccurred", Status: "False", Reason: "ApplicationSetUpToDate"},
			{Type: "ResourcesUpToDate", Status: "True", Reason: "ApplicationSetUpToDate"},
		},
		CreatedAt: str("2026-11-04T05:58:13.000Z"),
	}
	if !reflect.DeepEqual(set, want) {
		gotJ, _ := json.Marshal(set)
		wantJ, _ := json.Marshal(want)
		t.Fatalf("set:\n got %s\nwant %s", gotJ, wantJ)
	}

	var names []string
	for _, a := range d.Applications {
		names = append(names, a.Name)
	}
	if !reflect.DeepEqual(names, []string{"back-end-prod", "direct-app", "front-end-prod"}) {
		t.Fatalf("applications: %v", names)
	}
	be := d.Applications[0]
	if be.ApplicationSet == nil || *be.ApplicationSet != "nonprod-application-set" || be.SyncStatus != "Synced" ||
		be.HealthStatus != "Healthy" || !reflect.DeepEqual(be.SyncRevisions, []string{"de2287f9", "9e600687"}) ||
		be.OperationPhase == nil || *be.OperationPhase != "Succeeded" || be.ReconciledAt == nil ||
		*be.ReconciledAt != "2026-11-10T06:11:21.000Z" || !reflect.DeepEqual(be.Sources, wantSources) {
		t.Fatalf("generated app: %+v", be)
	}
	direct := d.Applications[1]
	if direct.ApplicationSet != nil || direct.DestinationName == nil || *direct.DestinationName != "in-cluster" ||
		direct.DestinationServer != nil || len(direct.Sources) != 1 || *direct.Sources[0].Chart != "app" ||
		direct.Sources[0].TargetRevision != "0.17.1" || !reflect.DeepEqual(direct.SyncRevisions, []string{"0.17.1"}) ||
		direct.OperationPhase != nil || direct.ReconciledAt != nil {
		t.Fatalf("direct app: %+v", direct)
	}

	raw, _ := json.Marshal(section)
	assertNoMarkers(t, "section", string(raw))
	assertNoMarkers(t, "logs", logs.String())
}

func TestArgoCDRootMissingIsAnEmptyOK(t *testing.T) {
	log, _ := testLogger()
	section, _ := ArgoCD(context.Background(), argoClient(tenantSet("nonprod-application-set")), argoParams, log)
	if section.Status != payload.StatusOK || section.Data.RootFound ||
		section.Data.ApplicationSets == nil || len(section.Data.ApplicationSets) != 0 ||
		section.Data.Applications == nil || len(section.Data.Applications) != 0 {
		t.Fatalf("unexpected: %+v", section)
	}
	raw, _ := json.Marshal(section)
	if !strings.Contains(string(raw), `"application_sets":[]`) || !strings.Contains(string(raw), `"applications":[]`) {
		t.Fatalf("empty lists must serialise as []: %s", raw)
	}
}

func TestArgoCDErrors(t *testing.T) {
	forbidden := apierrors.NewForbidden(k8sschema.GroupResource{Group: "argoproj.io", Resource: "applications"}, "", errors.New("denied"))
	for name, c := range map[string]struct {
		verb, resource string
		err            error
		want           string
	}{
		"root get denied":           {"get", "applications", forbidden, payload.ErrRBACDenied},
		"application list denied":   {"list", "applications", forbidden, payload.ErrRBACDenied},
		"set list denied":           {"list", "applicationsets", forbidden, payload.ErrRBACDenied},
		"application list unavail.": {"list", "applications", apierrors.NewServiceUnavailable("down"), payload.ErrAPIUnavailable},
	} {
		client := argoClient(venusLike()...)
		client.PrependReactor(c.verb, c.resource, func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, c.err })
		log, _ := testLogger()
		section, _ := ArgoCD(context.Background(), client, argoParams, log)
		if section.Status != payload.StatusError || section.Error != c.want || section.Data != nil {
			t.Errorf("%s: got %+v, want error/%s", name, section, c.want)
		}
	}

	log, _ := testLogger()
	if section, _ := ArgoCD(context.Background(), nil, argoParams, log); section.Error != payload.ErrAPIUnavailable {
		t.Errorf("nil client: got %+v", section)
	}
}

func TestArgoCDSkipsUndecodableObjects(t *testing.T) {
	objs := venusLike()
	bad := tenantApp("broken-app", "nonprod-application-set")
	bad.Object["spec"].(map[string]any)["project"] = int64(42) // not a string
	objs = append(objs, bad)
	log, logs := testLogger()
	section, skipped := ArgoCD(context.Background(), argoClient(objs...), argoParams, log)
	if section.Status != payload.StatusOK || skipped != 1 || len(section.Data.Applications) != 3 {
		t.Fatalf("unexpected: status=%s skipped=%d apps=%d", section.Status, skipped, len(section.Data.Applications))
	}
	if section.Data.ApplicationSets[0].ApplicationCount != 2 {
		t.Fatalf("a skipped app must not be counted: %+v", section.Data.ApplicationSets[0])
	}
	if !strings.Contains(logs.String(), `"msg":"argo cd object skipped"`) || !strings.Contains(logs.String(), `"name":"broken-app"`) {
		t.Fatalf("expected a skip warning:\n%s", logs.String())
	}
	assertNoMarkers(t, "logs", logs.String())
}

func TestStripUserinfo(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/GlueOps/x.git":                 "https://github.com/GlueOps/x.git",
		"https://deploy:tok@github.com/GlueOps/x.git":      "https://github.com/GlueOps/x.git",
		"https://tok@github.com/GlueOps/x.git":             "https://github.com/GlueOps/x.git",
		"git@github.com:development-captains/x.git":        "git@github.com:development-captains/x.git",
		"deploy:tok@github.com:development-captains/x.git": "github.com:development-captains/x.git",
		"ssh://git@github.com/org/x.git":                   "ssh://github.com/org/x.git",
		"oci://ghcr.io/glueops/charts":                     "oci://ghcr.io/glueops/charts",
		"ghcr.io/glueops/charts":                           "ghcr.io/glueops/charts",
		"":                                                 "",
	} {
		if got := StripUserinfo(in); got != want {
			t.Errorf("StripUserinfo(%q) = %q, want %q", in, got, want)
		}
	}
}
