// Package collect builds the inventory envelope from the Kubernetes API.
// Each dataset is collected independently; a failing dataset only sets its
// own section to status=error with a reason code.
package collect

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/glueops/inventory-agent/internal/logging"
	"github.com/glueops/inventory-agent/internal/schema"
)

// Params are the run-level inputs for an envelope.
type Params struct {
	CaptainDomain        string
	PlatformChartVersion string
	CollectorVersion     string
	RunID                string
	HelmNamespace        string
	PodNamespaces        []string
	MaxPodRows           int
}

// Envelope collects all datasets and assembles the run envelope. client may
// be nil, in which case every section reports api_unavailable.
func Envelope(ctx context.Context, client kubernetes.Interface, p Params, now time.Time, log *slog.Logger) schema.Envelope {
	env := schema.Envelope{
		SchemaVersion:        schema.EnvelopeSchemaVersion,
		CaptainDomain:        p.CaptainDomain,
		RunID:                p.RunID,
		CollectedAt:          FormatTime(now),
		CollectorVersion:     p.CollectorVersion,
		PlatformChartVersion: p.PlatformChartVersion,
	}

	if uid, err := ClusterUID(ctx, client); err != nil {
		log.Warn("cluster uid unavailable", "reason", logging.ReasonClusterUIDUnavailable, "cause", Classify(err), "error", err.Error())
	} else {
		env.ClusterUID = &uid
	}

	env.Datasets.Cluster = guard(log, "cluster", func() schema.ClusterSection {
		return Cluster(ctx, client, log)
	}, func(code string) schema.ClusterSection {
		return schema.ClusterSection{SchemaVersion: schema.ClusterSchemaVersion, Status: schema.StatusError, Error: code}
	})

	env.Datasets.HelmReleases = guard(log, "helm_releases", func() schema.HelmReleasesSection {
		return HelmReleases(ctx, client, p.HelmNamespace, log)
	}, func(code string) schema.HelmReleasesSection {
		return schema.HelmReleasesSection{SchemaVersion: schema.HelmReleasesSchemaVersion, Status: schema.StatusError, Error: code}
	})

	env.Datasets.PodImages = guard(log, "pod_images", func() schema.PodImagesSection {
		return PodImages(ctx, client, PodImagesParams{Namespaces: p.PodNamespaces, MaxRows: p.MaxPodRows}, log)
	}, func(code string) schema.PodImagesSection {
		return schema.PodImagesSection{
			SchemaVersion:       schema.PodImagesSchemaVersion,
			Status:              schema.StatusError,
			Error:               code,
			NamespacesRequested: append([]string{}, p.PodNamespaces...),
			NamespacesDenied:    []string{},
		}
	})

	return env
}

// guard runs a section collector and converts a panic into an
// internal_error section so one dataset can never take down the run.
func guard[T any](log *slog.Logger, name string, run func() T, onPanic func(code string) T) (out T) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("section panicked", "section", name, "reason", schema.ErrInternal, "panic", panicString(r))
			out = onPanic(schema.ErrInternal)
		}
	}()
	return run()
}

func panicString(r any) string {
	if err, ok := r.(error); ok {
		return err.Error()
	}
	if s, ok := r.(string); ok {
		return s
	}
	return "unknown panic"
}
