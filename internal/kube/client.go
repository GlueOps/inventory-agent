// Package kube builds the Kubernetes clientset: in-cluster when possible,
// otherwise from KUBECONFIG or ~/.kube/config for local runs.
package kube

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Clients are the typed clientset and the dynamic client (for Argo CD's
// custom resources), built from the same config.
type Clients struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
}

// NewClient returns both clients. userAgent is set on every API request.
func NewClient(userAgent string, timeout time.Duration) (Clients, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		path := os.Getenv("KUBECONFIG")
		if path == "" {
			home, herr := os.UserHomeDir()
			if herr != nil {
				return Clients{}, fmt.Errorf("not in cluster and no KUBECONFIG: %w", err)
			}
			path = filepath.Join(home, ".kube", "config")
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return Clients{}, fmt.Errorf("not in cluster and kubeconfig unusable: %w", err)
		}
	}
	cfg.UserAgent = userAgent
	cfg.Timeout = timeout
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return Clients{}, err
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return Clients{}, err
	}
	return Clients{Kube: kc, Dynamic: dc}, nil
}
