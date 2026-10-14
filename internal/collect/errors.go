package collect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/glueops/inventory-agent/internal/schema"
)

// errNoClient is returned when the agent has no Kubernetes client at all
// (for example when neither in-cluster config nor a kubeconfig was found).
var errNoClient = errors.New("kubernetes client unavailable")

// DecodeError marks a failure to decode a Helm release Secret. Its Error()
// deliberately contains only the Secret name and the stage that failed, never
// any of the decoded content, so it is safe to log.
type DecodeError struct {
	Secret string
	Stage  string
	Err    error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("decode release secret %q: %s failed", e.Secret, e.Stage)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// Classify maps an error to one of the fixed section error codes.
//
//	403/401                                  -> rbac_denied
//	connection, timeout, 5xx, 429, no client -> api_unavailable
//	DecodeError                              -> decode_failed
//	anything else                            -> internal_error
func Classify(err error) string {
	if err == nil {
		return ""
	}
	var de *DecodeError
	if errors.As(err, &de) {
		return schema.ErrDecodeFailed
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return schema.ErrRBACDenied
	}
	if errors.Is(err, errNoClient) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) {
		return schema.ErrAPIUnavailable
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return schema.ErrAPIUnavailable
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return schema.ErrAPIUnavailable
	}
	if apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsUnexpectedServerError(err) {
		return schema.ErrAPIUnavailable
	}
	if status, ok := err.(apierrors.APIStatus); ok || errors.As(err, &status) {
		if code := status.Status().Code; code >= 500 {
			return schema.ErrAPIUnavailable
		}
	}
	return schema.ErrInternal
}
