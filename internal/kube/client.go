// Package kube builds the Kubernetes clientset: in-cluster when possible,
// otherwise from KUBECONFIG or ~/.kube/config for local runs.
package kube

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClient returns a clientset. userAgent is set on every API request.
func NewClient(userAgent string, timeout time.Duration) (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		path := os.Getenv("KUBECONFIG")
		if path == "" {
			home, herr := os.UserHomeDir()
			if herr != nil {
				return nil, fmt.Errorf("not in cluster and no KUBECONFIG: %w", err)
			}
			path = filepath.Join(home, ".kube", "config")
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return nil, fmt.Errorf("not in cluster and kubeconfig unusable: %w", err)
		}
	}
	cfg.UserAgent = userAgent
	cfg.Timeout = timeout
	return kubernetes.NewForConfig(cfg)
}
