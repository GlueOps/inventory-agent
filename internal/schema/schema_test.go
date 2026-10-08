package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const schemaPath = "../../schema/payload.schema.json"

func loadSchema(t *testing.T) jsonSchema {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(schemaPath))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var s jsonSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	return s
}

func str(s string) *string { return &s }

// SampleEnvelope is a fully populated envelope exercising every field,
// including nulls and an errored section.
func SampleEnvelope() Envelope {
	return Envelope{
		SchemaVersion:        EnvelopeSchemaVersion,
		CaptainDomain:        "nonprod.foobar.onglueops.com",
		ClusterUID:           str("7897eb25-7a75-4903-b0f2-b8a849977428"),
		RunID:                "20261001050000_3f9c2a7e",
		CollectedAt:          "2026-10-01T05:00:00.412Z",
		CollectorVersion:     "v0.1.0",
		PlatformChartVersion: "0.80.2",
		Datasets: Datasets{
			Cluster: ClusterSection{
				SchemaVersion: ClusterSchemaVersion,
				Status:        StatusOK,
				Data: &ClusterData{
					K8sVersion: "v1.35.8+k3s1",
					K8sMajor:   "1",
					K8sMinor:   "35",
					Platform:   "linux/amd64",
					Nodes: []Node{{
						NodeName:                "k3d-captain-server-0",
						KubeletVersion:          "v1.35.8+k3s1",
						OSImage:                 "K3s v1.35.8+k3s1",
						OperatingSystem:         "linux",
						Architecture:            "amd64",
						KernelVersion:           "6.12.107+deb13-amd64",
						ContainerRuntimeVersion: "containerd://2.2.7-k3s1",
					}},
				},
			},
			HelmReleases: HelmReleasesSection{
				SchemaVersion: HelmReleasesSchemaVersion,
				Status:        StatusOK,
				Data: []HelmRelease{{
					ReleaseName:   "argocd",
					Namespace:     "glueops-core",
					ChartName:     "argo-cd",
					ChartVersion:  "10.2.2",
					AppVersion:    "v3.4.6",
					Revision:      1,
					Status:        "deployed",
					FirstDeployed: str("2026-09-29T15:53:59.278Z"),
					LastDeployed:  str("2026-09-29T15:53:59.278Z"),
				}},
			},
			PodImages: PodImagesSection{
				SchemaVersion:       PodImagesSchemaVersion,
				Status:              StatusOK,
				NamespacesRequested: []string{"kube-system", "glueops-core"},
				NamespacesDenied:    []string{},
				Truncated:           false,
				Data: []PodImage{
					{
						Namespace:     "kube-system",
						PodName:       "coredns-c5fdd76cf-bb6t2",
						PodPhase:      "Running",
						NodeName:      str("k3d-captain-agent-4"),
						OwnerKind:     str("ReplicaSet"),
						OwnerName:     str("coredns-c5fdd76cf"),
						WorkloadKind:  "Deployment",
						WorkloadName:  "coredns",
						ContainerName: "coredns",
						ContainerType: ContainerTypeApp,
						Image:         "rancher/mirrored-coredns-coredns:1.14.6",
						ImageID:       str("docker.io/rancher/mirrored-coredns-coredns@sha256:900f9c109f7a33545d3c811516e8376df9019147b750f5ce3e254468769176ea"),
						ImageDigest:   str("sha256:900f9c109f7a33545d3c811516e8376df9019147b750f5ce3e254468769176ea"),
					},
					{
						Namespace:     "glueops-core",
						PodName:       "pending-pod",
						PodPhase:      "Pending",
						NodeName:      nil,
						OwnerKind:     nil,
						OwnerName:     nil,
						WorkloadKind:  "Pod",
						WorkloadName:  "pending-pod",
						ContainerName: "init-wait",
						ContainerType: ContainerTypeInit,
						Image:         "busybox:1.36",
						ImageID:       nil,
						ImageDigest:   nil,
					},
				},
			},
		},
	}
}

func TestSampleEnvelopeValidatesAgainstSchema(t *testing.T) {
	s := loadSchema(t)
	for name, env := range map[string]Envelope{
		"ok": SampleEnvelope(),
		"errored sections": func() Envelope {
			e := SampleEnvelope()
			e.ClusterUID = nil
			e.Datasets.Cluster = ClusterSection{SchemaVersion: 1, Status: StatusError, Error: ErrAPIUnavailable}
			e.Datasets.HelmReleases = HelmReleasesSection{SchemaVersion: 1, Status: StatusError, Error: ErrRBACDenied}
			e.Datasets.PodImages = PodImagesSection{SchemaVersion: 1, Status: StatusError, Error: ErrInternal,
				NamespacesRequested: []string{"kube-system"}, NamespacesDenied: []string{}}
			return e
		}(),
		"empty lists": func() Envelope {
			e := SampleEnvelope()
			e.Datasets.HelmReleases.Data = []HelmRelease{}
			e.Datasets.PodImages.Data = []PodImage{}
			e.Datasets.Cluster.Data.Nodes = []Node{}
			return e
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(env)
			if err != nil {
				t.Fatal(err)
			}
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			if errs := validateJSON(s, s, v, "$"); len(errs) > 0 {
				t.Fatalf("envelope does not validate:\n%s", strings.Join(errs, "\n"))
			}
		})
	}
}

func TestSchemaRejectsExtraAndMissingFields(t *testing.T) {
	s := loadSchema(t)
	raw, _ := json.Marshal(SampleEnvelope())
	var v map[string]any
	_ = json.Unmarshal(raw, &v)

	// Extra field at top level.
	v["env"] = "CANARY_EXTRA_PROPERTY_VALUE"
	if errs := validateJSON(s, s, v, "$"); len(errs) == 0 {
		t.Fatal("expected top-level extra property to be rejected")
	}
	delete(v, "env")

	// Extra field inside a pod_images row (simulates a pod spec leak).
	rows := v["datasets"].(map[string]any)["pod_images"].(map[string]any)["data"].([]any)
	rows[0].(map[string]any)["env"] = []any{"CANARY_POD_ENV_VALUE"}
	if errs := validateJSON(s, s, v, "$"); len(errs) == 0 {
		t.Fatal("expected pod_images extra property to be rejected")
	}
	delete(rows[0].(map[string]any), "env")

	// Extra field inside a helm release (simulates values/manifest leak).
	rels := v["datasets"].(map[string]any)["helm_releases"].(map[string]any)["data"].([]any)
	rels[0].(map[string]any)["values"] = map[string]any{"password": "x"}
	if errs := validateJSON(s, s, v, "$"); len(errs) == 0 {
		t.Fatal("expected helm_releases extra property to be rejected")
	}
	delete(rels[0].(map[string]any), "values")

	// Missing required field.
	delete(v, "captain_domain")
	if errs := validateJSON(s, s, v, "$"); len(errs) == 0 {
		t.Fatal("expected missing captain_domain to be rejected")
	}
	v["captain_domain"] = "x"

	// Bad enum / pattern values.
	v["datasets"].(map[string]any)["cluster"].(map[string]any)["status"] = "maybe"
	if errs := validateJSON(s, s, v, "$"); len(errs) == 0 {
		t.Fatal("expected bad status enum to be rejected")
	}
	v["datasets"].(map[string]any)["cluster"].(map[string]any)["status"] = "ok"
	v["collected_at"] = "2026-10-01T05:00:00Z" // no milliseconds
	if errs := validateJSON(s, s, v, "$"); len(errs) == 0 {
		t.Fatal("expected non-millisecond timestamp to be rejected")
	}
}

// TestStructWithExtraFieldFails is the negative test: a struct type that
// grows an undeclared field must be caught by the schema.
func TestStructWithExtraFieldFails(t *testing.T) {
	s := loadSchema(t)
	type leakyPodImage struct {
		PodImage
		Env []string `json:"env"`
	}
	type leakySection struct {
		SchemaVersion       int             `json:"schema_version"`
		Status              string          `json:"status"`
		NamespacesRequested []string        `json:"namespaces_requested"`
		NamespacesDenied    []string        `json:"namespaces_denied"`
		Truncated           bool            `json:"truncated"`
		Data                []leakyPodImage `json:"data"`
	}
	base := SampleEnvelope()
	leaky := leakySection{
		SchemaVersion:       1,
		Status:              StatusOK,
		NamespacesRequested: []string{"kube-system"},
		NamespacesDenied:    []string{},
		Data:                []leakyPodImage{{PodImage: base.Datasets.PodImages.Data[0], Env: []string{"CANARY_POD_ENV_VALUE"}}},
	}
	raw, _ := json.Marshal(leaky)
	var v any
	_ = json.Unmarshal(raw, &v)
	sub, err := resolveRef(s, "#/$defs/pod_images_section")
	if err != nil {
		t.Fatal(err)
	}
	errs := validateJSON(s, sub, v, "$.datasets.pod_images")
	if len(errs) == 0 {
		t.Fatal("expected leaky struct to fail validation")
	}
	if !strings.Contains(strings.Join(errs, "\n"), `additional property "env"`) {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

// TestStructsAreClosed walks every payload type by reflection and asserts
// that each exported field has an explicit json tag and that no field is a
// map, interface or other open type.
func TestStructsAreClosed(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(rt reflect.Type, path string)
	walk = func(rt reflect.Type, path string) {
		switch rt.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(rt.Elem(), path)
			return
		case reflect.Map, reflect.Interface, reflect.Chan, reflect.Func, reflect.UnsafePointer:
			t.Errorf("%s: open type %s is not allowed in the payload", path, rt)
			return
		case reflect.Struct:
		default:
			return
		}
		if seen[rt] {
			return
		}
		seen[rt] = true
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			fp := path + "." + f.Name
			if f.Anonymous {
				t.Errorf("%s: embedded fields are not allowed", fp)
			}
			tag, ok := f.Tag.Lookup("json")
			if !ok || tag == "" || tag == "-" || strings.HasPrefix(tag, ",") {
				t.Errorf("%s: missing explicit json tag", fp)
			}
			walk(f.Type, fp)
		}
	}
	walk(reflect.TypeOf(Envelope{}), "Envelope")
}

// TestSchemaMatchesStructs asserts that the JSON Schema's property names are
// exactly the Go struct json tags at every level, so neither can drift.
func TestSchemaMatchesStructs(t *testing.T) {
	s := loadSchema(t)

	var check func(rt reflect.Type, node jsonSchema, path string)
	check = func(rt reflect.Type, node jsonSchema, path string) {
		node = deref(t, s, node)
		switch rt.Kind() {
		case reflect.Pointer:
			check(rt.Elem(), node, path)
		case reflect.Slice:
			items, ok := node["items"].(map[string]any)
			if !ok {
				t.Errorf("%s: schema has no items for slice", path)
				return
			}
			check(rt.Elem(), items, path+"[]")
		case reflect.Struct:
			props, _ := node["properties"].(map[string]any)
			if ap, ok := node["additionalProperties"].(bool); !ok || ap {
				t.Errorf("%s: additionalProperties must be false", path)
			}
			var want []string
			for i := 0; i < rt.NumField(); i++ {
				f := rt.Field(i)
				name := strings.Split(f.Tag.Get("json"), ",")[0]
				want = append(want, name)
				sub, ok := props[name]
				if !ok {
					t.Errorf("%s: schema missing property %q", path, name)
					continue
				}
				check(f.Type, sub.(map[string]any), path+"."+name)
			}
			var got []string
			for k := range props {
				got = append(got, k)
			}
			sort.Strings(want)
			sort.Strings(got)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("%s: schema properties %v != struct fields %v", path, got, want)
			}
		}
	}
	check(reflect.TypeOf(Envelope{}), s, "$")
}

// deref follows $ref and, for nullable anyOf wrappers, picks the non-null
// branch so the structural comparison can continue.
func deref(t *testing.T, root jsonSchema, node jsonSchema) jsonSchema {
	t.Helper()
	for i := 0; i < 5; i++ {
		if ref, ok := node["$ref"].(string); ok {
			n, err := resolveRef(root, ref)
			if err != nil {
				t.Fatal(err)
			}
			node = n
			continue
		}
		if anyOf, ok := node["anyOf"].([]any); ok {
			for _, sub := range anyOf {
				m := sub.(map[string]any)
				if ty, _ := m["type"].(string); ty == "null" {
					continue
				}
				node = m
				break
			}
			continue
		}
		break
	}
	return node
}
