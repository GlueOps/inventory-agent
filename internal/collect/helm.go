package collect

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/glueops/inventory-agent/internal/schema"
)

const (
	helmSecretType     = "helm.sh/release.v1"
	helmLabelSelector  = "owner=helm"
	helmReleaseDataKey = "release"
	// maxReleaseBytes bounds the decompressed release JSON so a hostile or
	// corrupt Secret (a gzip bomb) cannot exhaust memory. Real releases are
	// well under 1 MiB; Kubernetes itself caps a Secret at 1 MiB encoded.
	maxReleaseBytes = 16 << 20
)

// helmRelease is the closed view of Helm's release JSON. encoding/json drops
// every field not declared here, so values, manifest, hooks and description
// are never materialised as Go values.
type helmRelease struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Version   int    `json:"version"`
	Info      struct {
		Status        string `json:"status"`
		FirstDeployed string `json:"first_deployed"`
		LastDeployed  string `json:"last_deployed"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			AppVersion string `json:"appVersion"`
		} `json:"metadata"`
	} `json:"chart"`
}

// HelmReleases lists Helm release Secrets in namespace (label selector
// owner=helm, type helm.sh/release.v1), decodes only the "release" key and
// keeps the latest revision of each release.
//
// A Secret that cannot be decoded is skipped with a warning (secret name and
// failing stage only) and counted in skipped; the others are still
// reported. Only when every Helm Secret failed to decode is the section
// itself marked decode_failed.
func HelmReleases(ctx context.Context, client kubernetes.Interface, namespace string, log *slog.Logger) (section schema.HelmReleasesSection, skipped int) {
	section = schema.HelmReleasesSection{SchemaVersion: schema.HelmReleasesSchemaVersion}
	data, skipped, err := collectHelmReleases(ctx, client, namespace, log)
	if err != nil {
		section.Status = schema.StatusError
		section.Error = Classify(err)
		log.Warn("section failed", "section", "helm_releases", "reason", section.Error, "error", err.Error())
		return section, skipped
	}
	section.Status = schema.StatusOK
	section.Data = data
	return section, skipped
}

func collectHelmReleases(ctx context.Context, client kubernetes.Interface, namespace string, log *slog.Logger) ([]schema.HelmRelease, int, error) {
	if client == nil {
		return nil, 0, errNoClient
	}
	list, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: helmLabelSelector,
		FieldSelector: "type=" + helmSecretType,
	})
	if err != nil {
		return nil, 0, err
	}

	latest := map[string]schema.HelmRelease{}
	var seen, skipped int
	var lastErr error
	for i := range list.Items {
		sec := &list.Items[i]
		// The field selector is advisory (fakes ignore it); never decode a
		// non-Helm Secret.
		if string(sec.Type) != helmSecretType {
			continue
		}
		seen++
		rel, err := decodeReleaseSecret(sec)
		if err != nil {
			skipped++
			lastErr = err
			var de *DecodeError
			stage := "unknown"
			if errors.As(err, &de) {
				stage = de.Stage
			}
			log.Warn("release secret skipped", "section", "helm_releases", "reason", schema.ErrDecodeFailed,
				"secret", sec.Name, "stage", stage)
			continue
		}
		if cur, ok := latest[rel.ReleaseName]; !ok || rel.Revision > cur.Revision {
			latest[rel.ReleaseName] = rel
		}
	}
	if seen > 0 && skipped == seen {
		return nil, skipped, lastErr
	}

	out := make([]schema.HelmRelease, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReleaseName < out[j].ReleaseName })
	return out, skipped, nil
}

// decodeReleaseSecret extracts the nine metadata fields from one Helm
// release Secret.
//
// Helm's Secrets driver stores the release as base64(gzip(json)) in
// Secret.data["release"]. client-go has already undone the Kubernetes-level
// base64 of Secret.data, so the bytes we receive are the base64 *text*
// written by Helm: decode that, gunzip (older Helm versions may store plain
// JSON, which is accepted), then unmarshal into the closed helmRelease view.
func decodeReleaseSecret(sec *corev1.Secret) (schema.HelmRelease, error) {
	var zero schema.HelmRelease
	raw, ok := sec.Data[helmReleaseDataKey]
	if !ok || len(raw) == 0 {
		return zero, &DecodeError{Secret: sec.Name, Stage: "missing release key"}
	}

	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(raw)))
	n, err := base64.StdEncoding.Decode(decoded, bytes.TrimSpace(raw))
	if err != nil {
		return zero, &DecodeError{Secret: sec.Name, Stage: "base64", Err: err}
	}
	decoded = decoded[:n]

	var jsonBytes []byte
	if isGzip(decoded) {
		zr, err := gzip.NewReader(bytes.NewReader(decoded))
		if err != nil {
			return zero, &DecodeError{Secret: sec.Name, Stage: "gzip", Err: err}
		}
		var tooLarge bool
		jsonBytes, tooLarge, err = readBounded(zr, maxReleaseBytes)
		_ = zr.Close()
		if err != nil {
			return zero, &DecodeError{Secret: sec.Name, Stage: "gunzip", Err: err}
		}
		if tooLarge {
			return zero, &DecodeError{Secret: sec.Name, Stage: "release too large"}
		}
	} else {
		jsonBytes = decoded
	}

	var rel helmRelease
	if err := json.Unmarshal(jsonBytes, &rel); err != nil {
		return zero, &DecodeError{Secret: sec.Name, Stage: "json", Err: err}
	}
	if rel.Name == "" {
		return zero, &DecodeError{Secret: sec.Name, Stage: "json", Err: fmt.Errorf("release has no name")}
	}

	// Revision comes from the Secret's "version" label (what helm list
	// reads); fall back to the JSON field if the label is absent.
	revision := rel.Version
	if v, err := strconv.Atoi(sec.Labels["version"]); err == nil {
		revision = v
	}
	namespace := rel.Namespace
	if namespace == "" {
		namespace = sec.Namespace
	}

	return schema.HelmRelease{
		ReleaseName:   rel.Name,
		Namespace:     namespace,
		ChartName:     rel.Chart.Metadata.Name,
		ChartVersion:  rel.Chart.Metadata.Version,
		AppVersion:    rel.Chart.Metadata.AppVersion,
		Revision:      revision,
		Status:        rel.Info.Status,
		FirstDeployed: formatTimePtr(parseHelmTime(rel.Info.FirstDeployed)),
		LastDeployed:  formatTimePtr(parseHelmTime(rel.Info.LastDeployed)),
	}, nil
}

// readBounded reads at most max bytes from r. It reports tooLarge as soon
// as a byte beyond max is seen, so the allocation is bounded by max+1 and
// the buffer grows by plain doubling (about 2x max total allocation in the
// worst case) instead of io.ReadAll's growth pattern.
func readBounded(r io.Reader, max int) (data []byte, tooLarge bool, err error) {
	buf := make([]byte, 0, 64<<10)
	for {
		if len(buf) == cap(buf) {
			next := cap(buf) * 2
			if next > max+1 {
				next = max + 1
			}
			grown := make([]byte, len(buf), next)
			copy(grown, buf)
			buf = grown
		}
		n, rerr := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if len(buf) > max {
			return nil, true, nil
		}
		if rerr == io.EOF {
			return buf, false, nil
		}
		if rerr != nil {
			return nil, false, rerr
		}
	}
}

func isGzip(b []byte) bool {
	return len(b) > 3 && b[0] == 0x1f && b[1] == 0x8b && b[2] == 0x08
}
