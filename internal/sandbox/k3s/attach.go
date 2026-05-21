package k3s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// eligibleForPool reports whether a RunRequest can be served from
// the warm pod pool. Pool pods are sized with defaults + no custom
// image + no packages + network=none. Any divergence falls through
// to the per-run create path which honors the request as-is.
func eligibleForPool(req sandbox.RunRequest) bool {
	return req.Image == "" &&
		len(req.Packages) == 0 &&
		!req.Network &&
		req.MemLimit == sandbox.DefaultMemLimit &&
		req.CPULimit == sandbox.DefaultCPULimit
}

// attachAndStream attaches to the named pod's main container, writes the JSON
// payload to its stdin, and streams stdout/stderr through the provided writers.
// Blocks until the container exits (stdin EOF on the entrypoint).
//
// Returns the container exit code if known, plus any error from the stream.
func (r *Runtime) attachAndStream(
	ctx context.Context,
	podName string,
	payload []byte,
	stdoutW, stderrW io.Writer,
) (exitCode int32, err error) {
	if err := waitPodRunning(ctx, r.clientset, r.ns, podName, 90*time.Second); err != nil {
		return -1, err
	}

	req := r.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(r.ns).
		Name(podName).
		SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{
			Container: "user",
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(r.restConfig, "POST", req.URL())
	if err != nil {
		return -1, fmt.Errorf("sandbox/k3s: spdy executor: %w", err)
	}

	stdinR, stdinW := io.Pipe()
	go func() {
		defer stdinW.Close()
		_, _ = stdinW.Write(payload)
	}()

	streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdinR,
		Stdout: stdoutW,
		Stderr: stderrW,
		Tty:    false,
	})

	// The attach stream closes as soon as the user process exits, but the pod
	// status may not have transitioned to Terminated yet. Poll briefly so the
	// caller sees the real exit code instead of a spurious -1.
	if code, ok := waitForTerminated(ctx, r, podName, 5*time.Second); ok {
		return code, streamErr
	}
	return -1, streamErr
}

// waitForTerminated polls the pod until the "user" container reports
// a Terminated state or the deadline elapses.
//
// Polling cadence is tight (10ms initial, 50ms cap) because this
// runs AFTER the attach stream has already closed — the user
// process is provably done; we're just waiting for kubelet to
// reflect the terminated state. With exponential backoff (50ms →
// 800ms) a single missed-by-a-tick check stretched skill calls
// to 1.5s+, dominating the runtime for sub-second skills.
func waitForTerminated(ctx context.Context, r *Runtime, podName string, maxWait time.Duration) (int32, bool) {
	deadline := time.Now().Add(maxWait)
	delay := 10 * time.Millisecond
	for {
		pod, err := r.clientset.CoreV1().Pods(r.ns).Get(ctx, podName, metav1.GetOptions{})
		if err == nil {
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.Name == "user" && cs.State.Terminated != nil {
					return cs.State.Terminated.ExitCode, true
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		select {
		case <-ctx.Done():
			return 0, false
		case <-time.After(delay):
		}
		if delay < 50*time.Millisecond {
			delay *= 2
		}
	}
}

// Run executes user code in a one-shot pod and returns the buffered result.
func (r *Runtime) Run(ctx context.Context, req sandbox.RunRequest) (*sandbox.RunResult, error) {
	applyDefaults(&req)

	image, err := r.resolveImage(ctx, req, false)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(map[string]any{
		"code":    req.Code,
		"input":   req.Input,
		"context": req.Context,
		"params":  req.Params,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox/k3s: marshal payload: %w", err)
	}

	// Pool fast-path: when the request matches the warm-pod
	// envelope (default mem/cpu, no custom image, no packages, no
	// network) and the pool has a ready pod, skip the create +
	// wait-for-running overhead.
	var podName string
	var fromPool bool
	if r.pool != nil && eligibleForPool(req) {
		if name := r.pool.Acquire(req.Language); name != "" {
			podName = name
			fromPool = true
			slog.Info("sandbox/k3s: pool acquired", "pod", name, "language", req.Language)
		}
	}
	if !fromPool {
		slog.Info("sandbox/k3s: cold start (no pool match)", "language", req.Language, "has_packages", len(req.Packages) > 0, "custom_image", req.Image != "")
		pod := r.buildPod(req, image, false)
		created, err := r.clientset.CoreV1().Pods(r.ns).Create(ctx, pod, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("sandbox/k3s: create pod: %w", err)
		}
		podName = created.Name
	}
	defer func() {
		if fromPool {
			r.pool.Release(context.Background(), podName)
		} else {
			deletePod(context.Background(), r.clientset, r.ns, podName)
		}
	}()

	start := time.Now()
	var stdoutBuf, stderrBuf bytes.Buffer

	execCtx, cancel := context.WithTimeout(ctx, req.Timeout+10*time.Second) // allow pod scheduling overhead
	defer cancel()

	exitCode, streamErr := r.attachAndStream(execCtx, podName, payload, &stdoutBuf, &stderrBuf)
	if streamErr != nil && exitCode == -1 {
		return nil, fmt.Errorf("sandbox/k3s: attach: %w", streamErr)
	}

	stdout := stdoutBuf.String()
	stderr := stderrBuf.String()

	var output any
	if trimmed := strings.TrimSpace(stdout); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &output); err != nil {
			output = trimmed
		}
	}

	return &sandbox.RunResult{
		Output:   output,
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: int(exitCode),
		Duration: time.Since(start),
	}, nil
}

// StreamRun executes user code, streaming stdout/stderr in real-time over the
// returned channel. The channel closes after an "exit" event.
func (r *Runtime) StreamRun(ctx context.Context, req sandbox.RunRequest) (<-chan sandbox.OutputEvent, error) {
	applyDefaults(&req)

	image, err := r.resolveImage(ctx, req, false)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(map[string]any{
		"code":    req.Code,
		"input":   req.Input,
		"context": req.Context,
		"params":  req.Params,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox/k3s: marshal payload: %w", err)
	}

	// Same pool fast-path as Run — eligible requests skip pod
	// creation; ineligible requests fall through to per-run create.
	var podName string
	var fromPool bool
	if r.pool != nil && eligibleForPool(req) {
		if name := r.pool.Acquire(req.Language); name != "" {
			podName = name
			fromPool = true
			slog.Info("sandbox/k3s: pool acquired (stream)", "pod", name, "language", req.Language)
		}
	}
	if !fromPool {
		pod := r.buildPod(req, image, false)
		created, err := r.clientset.CoreV1().Pods(r.ns).Create(ctx, pod, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("sandbox/k3s: create pod: %w", err)
		}
		podName = created.Name
	}

	ch := make(chan sandbox.OutputEvent, 64)

	go func() {
		defer close(ch)
		defer func() {
			if fromPool {
				r.pool.Release(context.Background(), podName)
			} else {
				deletePod(context.Background(), r.clientset, r.ns, podName)
			}
		}()

		stdoutW := &sandbox.StreamWriter{Stream: "stdout", Ch: ch}
		stderrW := &sandbox.StreamWriter{Stream: "stderr", Ch: ch}

		execCtx, cancel := context.WithTimeout(ctx, req.Timeout+10*time.Second)
		defer cancel()

		start := time.Now()
		exitCode, streamErr := r.attachAndStream(execCtx, podName, payload, stdoutW, stderrW)

		if streamErr != nil && exitCode == -1 {
			ch <- sandbox.OutputEvent{Stream: "exit", Error: streamErr.Error()}
			return
		}

		ch <- sandbox.OutputEvent{
			Stream:   "exit",
			ExitCode: int(exitCode),
			Duration: time.Since(start).Round(time.Millisecond).String(),
		}
	}()

	return ch, nil
}

// applyDefaults fills missing fields on a RunRequest.
func applyDefaults(req *sandbox.RunRequest) {
	if req.Timeout == 0 {
		req.Timeout = sandbox.DefaultTimeout
	}
	if req.MemLimit == 0 {
		req.MemLimit = sandbox.DefaultMemLimit
	}
	if req.CPULimit == 0 {
		req.CPULimit = sandbox.DefaultCPULimit
	}
}

