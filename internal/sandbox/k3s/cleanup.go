package k3s

import (
	"context"
	"log/slog"

	"github.com/bRRRITSCOLD/burrow/internal/sandbox"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CleanupOrphans deletes any sandbox-labeled pods left over from a previous
// crashed session. Mirrors the Docker backend behavior; safe to call once at
// startup before serving traffic.
func (r *Runtime) CleanupOrphans(ctx context.Context) {
	pods, err := r.clientset.CoreV1().Pods(r.ns).List(ctx, metav1.ListOptions{
		LabelSelector: sandbox.SandboxLabel + "=true",
	})
	if err != nil {
		slog.Warn("sandbox/k3s: orphan list failed", "err", err)
		return
	}
	if len(pods.Items) == 0 {
		return
	}
	slog.Info("sandbox/k3s: cleaning up orphan pods", "count", len(pods.Items))
	zero := int64(0)
	for _, p := range pods.Items {
		_ = r.clientset.CoreV1().Pods(r.ns).Delete(ctx, p.Name, metav1.DeleteOptions{
			GracePeriodSeconds: &zero,
		})
		slog.Info("sandbox/k3s: removed orphan", "pod", p.Name)
	}
}
