package k3s

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// sandboxUID is the in-image uid for the sandbox user (set in all runtime Dockerfiles).
const sandboxUID int64 = 10001

// randID returns a 10-char hex id suitable for pod name suffixes.
func randID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// imageWithRegistry prefixes a base image with the configured registry.
// Already-prefixed (registry contains "." or ":") images pass through.
func (r *Runtime) imageWithRegistry(img string) string {
	if img == "" {
		return img
	}
	first := img
	if i := strings.Index(img, "/"); i >= 0 {
		first = img[:i]
	}
	if strings.ContainsAny(first, ".:") {
		// already has registry-like prefix
		return img
	}
	return r.registry + "/" + img
}

// resolveImage returns the registry-prefixed image to use for a request.
// Priority: custom_image > packages (build + push) > base (registry-prefixed).
func (r *Runtime) resolveImage(ctx context.Context, req sandbox.RunRequest, debug bool) (string, error) {
	if req.Image != "" {
		return r.imageWithRegistry(req.Image), nil
	}
	var base string
	if debug {
		base = sandbox.DebugImageForLanguage(req.Language)
	} else {
		base = sandbox.ImageForLanguage(req.Language)
	}
	if base == "" {
		return "", fmt.Errorf("sandbox/k3s: unsupported language: %s", req.Language)
	}
	prefixedBase := r.imageWithRegistry(base)
	if len(req.Packages) > 0 {
		// Builder uses the registry-prefixed base so derived tags also include the prefix,
		// matching what k3s containerd will pull.
		return r.bldr.resolvePackageImage(ctx, req.Language, prefixedBase, req.Packages, debug)
	}
	return prefixedBase, nil
}

// buildPod constructs a Pod spec for one user-script execution.
func (r *Runtime) buildPod(req sandbox.RunRequest, image string, debugMode bool) *corev1.Pod {
	memBytes := req.MemLimit
	if memBytes == 0 {
		memBytes = sandbox.DefaultMemLimit
	}
	cpuMillis := int64(req.CPULimit * 1000)
	if cpuMillis == 0 {
		cpuMillis = int64(sandbox.DefaultCPULimit * 1000)
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = sandbox.DefaultTimeout
	}
	deadlineSec := int64(timeout.Seconds())
	if deadlineSec < 1 {
		deadlineSec = 1
	}

	netLabelVal := netLabelDeny
	if req.Network {
		netLabelVal = netLabelAllow
	}

	labels := map[string]string{
		sandbox.SandboxLabel:           "true",
		sandbox.SandboxLabel + "/lang": string(req.Language),
		netLabel:                       netLabelVal,
	}

	name := "sandbox-" + randID()
	if debugMode {
		name = "sandbox-debug-" + randID()
	}

	// stdinOnce=true: k8s closes stdin after first attach disconnects → entrypoint
	// sees EOF → unblocks `stdin.on('end')` and proceeds to spawn debug child.
	// With stdinOnce=false, the pipe stays open empty across attaches and the
	// wrapper hangs waiting for EOF that never arrives.
	stdinOnce := true

	// Always pull so dev rebuilds reflect immediately in both Run and Debug
	// pods. Cost is one registry HEAD per Pod create — negligible over the
	// loopback registry.
	container := corev1.Container{
		Name:            "user",
		Image:           image,
		ImagePullPolicy: corev1.PullAlways,
		Stdin:           true,
		StdinOnce:       stdinOnce,
		TTY:             false,
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceMemory:           *resource.NewQuantity(memBytes, resource.BinarySI),
				corev1.ResourceCPU:              *resource.NewMilliQuantity(cpuMillis, resource.DecimalSI),
				corev1.ResourceEphemeralStorage: resource.MustParse("256Mi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: *resource.NewQuantity(memBytes/2, resource.BinarySI),
				corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMillis/2, resource.DecimalSI),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptrBool(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			RunAsNonRoot:             ptrBool(true),
			RunAsUser:                ptrInt64(sandboxUID),
		},
	}

	if debugMode {
		container.Env = []corev1.EnvVar{{Name: "SANDBOX_DEBUG", Value: "1"}}
	}

	spec := corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		ActiveDeadlineSeconds:        &deadlineSec,
		AutomountServiceAccountToken: ptrBool(false),
		EnableServiceLinks:           ptrBool(false),
		Containers:                   []corev1.Container{container},
	}
	// gVisor's netstack mishandles kubelet portforward (which dials
	// localhost:port inside the pod netns) — connect refused even when the
	// process binds 0.0.0.0. Skip RuntimeClass for debug pods so they run
	// under the default (runc) runtime; debug sessions are dev-only and the
	// security ceiling for non-prod is acceptable. Run pods stay on gVisor.
	if !debugMode {
		spec.RuntimeClassName = ptrString(r.runtimeClass)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.ns,
			Labels:    labels,
		},
		Spec: spec,
	}
	return pod
}

// waitPodRunning polls the API until the pod's main container reports Running,
// or the context/timeout expires. On timeout, includes the latest container
// status reason/message in the error so callers can tell ImagePullBackOff
// from CrashLoopBackOff at a glance.
func waitPodRunning(ctx context.Context, cli kubernetes.Interface, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastReason, lastMessage string
	for {
		if time.Now().After(deadline) {
			if lastReason != "" {
				return fmt.Errorf("sandbox/k3s: pod %s did not reach Running within %s: %s — %s",
					name, timeout, lastReason, lastMessage)
			}
			return fmt.Errorf("sandbox/k3s: pod %s did not reach Running within %s", name, timeout)
		}
		pod, err := cli.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return err
		}
		if pod.Status.Phase == corev1.PodRunning {
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.Name == "user" && (cs.Ready || cs.State.Running != nil) {
					return nil
				}
			}
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return fmt.Errorf("sandbox/k3s: pod %s entered terminal phase %s before attach", name, pod.Status.Phase)
		}
		// Capture latest container Waiting state for diagnostics.
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "user" && cs.State.Waiting != nil {
				lastReason = cs.State.Waiting.Reason
				lastMessage = cs.State.Waiting.Message
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// deletePod force-removes a pod (zero grace period).
func deletePod(ctx context.Context, cli kubernetes.Interface, ns, name string) {
	zero := int64(0)
	_ = cli.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &zero,
	})
}

func ptrBool(b bool) *bool       { return &b }
func ptrInt64(v int64) *int64    { return &v }
func ptrString(s string) *string { return &s }
