package k3s

import (
	"context"
	"fmt"

	"github.com/bRRRITSCOLD/burrow/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ensureNamespace creates the sandbox namespace if missing. Idempotent.
func ensureNamespace(ctx context.Context, cli kubernetes.Interface, ns string) error {
	_, err := cli.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	_, err = cli.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
			Labels: map[string]string{
				sandbox.SandboxLabel: "true",
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// ensureRuntimeClass installs the gVisor RuntimeClass if missing. Idempotent.
func ensureRuntimeClass(ctx context.Context, cli kubernetes.Interface, name string) error {
	_, err := cli.NodeV1().RuntimeClasses().Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	rc := &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Handler:    "runsc",
	}
	_, err = cli.NodeV1().RuntimeClasses().Create(ctx, rc, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create RuntimeClass %s: %w", name, err)
	}
	return nil
}
